package storage

import (
	"context"
	"sort"
	"time"

	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"

	"github.com/grafana/loki/v3/pkg/chunkenc"
	"github.com/grafana/loki/v3/pkg/iter"
	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/grafana/loki/v3/pkg/logqlmodel/stats"
	"github.com/grafana/loki/v3/pkg/querier/astmapper"
	"github.com/grafana/loki/v3/pkg/storage/chunk"
	"github.com/grafana/loki/v3/pkg/storage/chunk/fetcher"
	"github.com/grafana/loki/v3/pkg/storage/config"
	"github.com/grafana/loki/v3/pkg/util/constants"
	util_log "github.com/grafana/loki/v3/pkg/util/log"
)

type ChunkMetrics struct {
	refs         *prometheus.CounterVec
	refsBypassed prometheus.Counter
	series       *prometheus.CounterVec
	chunks       *prometheus.CounterVec
	batches      *prometheus.HistogramVec
}

const (
	statusDiscarded = "discarded"
	statusMatched   = "matched"
)

func NewChunkMetrics(r prometheus.Registerer, maxBatchSize int) *ChunkMetrics {
	buckets := 5
	if maxBatchSize < buckets {
		maxBatchSize = buckets
	}

	return &ChunkMetrics{
		refs: promauto.With(r).NewCounterVec(prometheus.CounterOpts{
			Namespace: constants.Loki,
			Subsystem: "index",
			Name:      "chunk_refs_total",
			Help:      "Number of chunks refs downloaded, partitioned by whether they intersect the query bounds.",
		}, []string{"status"}),
		refsBypassed: promauto.With(r).NewCounter(prometheus.CounterOpts{
			Namespace: constants.Loki,
			Subsystem: "store",
			Name:      "chunk_ref_lookups_bypassed_total",
			Help:      "Number of chunk refs that were bypassed due to store overrides: computed during planning to avoid lookups",
		}),
		series: promauto.With(r).NewCounterVec(prometheus.CounterOpts{
			Namespace: constants.Loki,
			Subsystem: "store",
			Name:      "series_total",
			Help:      "Number of series referenced by a query, partitioned by whether they satisfy matchers.",
		}, []string{"status"}),
		chunks: promauto.With(r).NewCounterVec(prometheus.CounterOpts{
			Namespace: constants.Loki,
			Subsystem: "store",
			Name:      "chunks_downloaded_total",
			Help:      "Number of chunks referenced or downloaded, partitioned by if they satisfy matchers.",
		}, []string{"status"}),
		batches: promauto.With(r).NewHistogramVec(prometheus.HistogramOpts{
			Namespace: constants.Loki,
			Subsystem: "store",
			Name:      "chunks_per_batch",
			Help:      "The chunk batch size, partitioned by if they satisfy matchers.",

			// split buckets evenly across 0->maxBatchSize
			Buckets: prometheus.LinearBuckets(0, float64(maxBatchSize/buckets), buckets+1), // increment buckets by one to ensure upper bound bucket exists.
		}, []string{"status"}),
	}
}

// batchChunkIterator iterates through chunks by batch of `batchSize`.
// Since chunks can overlap across batches for each iteration the iterator will keep all overlapping
// chunks with the next chunk from the next batch and added it to the next iteration. In this case the boundaries of the batch
// is reduced to non-overlapping chunks boundaries.
type batchChunkIterator struct {
	schemas         config.SchemaConfig
	chunks          lazyChunks
	batchSize       int
	lastOverlapping []*LazyChunk
	metrics         *ChunkMetrics
	matchers        []*labels.Matcher
	chunkFilterer   chunk.Filterer

	begun      bool
	ctx        context.Context
	start, end time.Time
	direction  logproto.Direction
	next       chan *chunkBatch
}

// newBatchChunkIterator creates a new batch iterator with the given batchSize.
func newBatchChunkIterator(
	ctx context.Context,
	s config.SchemaConfig,
	chunks []*LazyChunk,
	batchSize int,
	direction logproto.Direction,
	start, end time.Time,
	metrics *ChunkMetrics,
	matchers []*labels.Matcher,
	chunkFilterer chunk.Filterer,
) *batchChunkIterator {
	// __name__ is not something we filter by because it's a constant in loki
	// and only used for upstream compatibility; therefore remove it.
	// The same applies to the sharding label which is injected by the cortex storage code.
	matchers = removeMatchersByName(matchers, labels.MetricName, astmapper.ShardLabel)
	res := &batchChunkIterator{
		batchSize:     batchSize,
		schemas:       s,
		metrics:       metrics,
		matchers:      matchers,
		start:         start,
		end:           end,
		direction:     direction,
		ctx:           ctx,
		chunks:        lazyChunks{direction: direction, chunks: chunks},
		next:          make(chan *chunkBatch),
		chunkFilterer: chunkFilterer,
	}
	sort.Sort(res.chunks)
	return res
}

// Start is idempotent and will begin the processing thread which seeds the iterator data.
func (it *batchChunkIterator) Start() {
	if !it.begun {
		it.begun = true
		go it.loop()
	}
}

func (it *batchChunkIterator) loop() {
	for {
		if it.chunks.Len() == 0 {
			close(it.next)
			return
		}
		select {
		case <-it.ctx.Done():
			close(it.next)
			return
		case it.next <- it.nextBatch():
		}
	}
}

func (it *batchChunkIterator) Next() *chunkBatch {
	it.Start() // Ensure the iterator has started.
	return <-it.next
}

func (it *batchChunkIterator) nextBatch() (res *chunkBatch) {
	defer func() {
		if p := recover(); p != nil {
			level.Error(util_log.Logger).Log("msg", "panic while fetching chunks", "panic", p)
			res = &chunkBatch{
				err: errors.Errorf("panic while fecthing chunks %+v", p),
			}
		}
	}()
	// the first chunk of the batch
	headChunk := it.chunks.Peek()
	from, through := it.start, it.end
	batch := make([]*LazyChunk, 0, it.batchSize+len(it.lastOverlapping))
	var nextChunk *LazyChunk

	var includesOverlap bool

	for it.chunks.Len() > 0 {

		// pop the next batch of chunks and append/prepend previous overlapping chunks
		// so we can merge/de-dupe overlapping entries.
		if !includesOverlap && it.direction == logproto.FORWARD {
			batch = append(batch, it.lastOverlapping...)
		}
		batch = append(batch, it.chunks.pop(it.batchSize)...)
		if !includesOverlap && it.direction == logproto.BACKWARD {
			batch = append(batch, it.lastOverlapping...)
		}

		includesOverlap = true

		if it.chunks.Len() > 0 {
			nextChunk = it.chunks.Peek()
			// we max out our iterator boundaries to the next chunks in the queue
			// so that overlapping chunks are together
			if it.direction == logproto.BACKWARD {
				from = time.Unix(0, nextChunk.Chunk.Through.UnixNano())

				// we have to reverse the inclusivity of the chunk iterator from
				// [from, through) to (from, through] for backward queries, except when
				// the batch's `from` is equal to the query's Start. This can be achieved
				// by shifting `from` by one nanosecond.
				if !from.Equal(it.start) {
					from = from.Add(time.Nanosecond)
				}
			} else {
				through = time.Unix(0, nextChunk.Chunk.From.UnixNano())
			}
			// we save all overlapping chunks as they are also needed in the next batch to properly order entries.
			// If we have chunks like below:
			//      ┌──────────────┐
			//      │     # 47     │
			//      └──────────────┘
			//          ┌──────────────────────────┐
			//          │           # 48           |
			//          └──────────────────────────┘
			//              ┌──────────────┐
			//              │     # 49     │
			//              └──────────────┘
			//                        ┌────────────────────┐
			//                        │        # 50        │
			//                        └────────────────────┘
			//
			//  And nextChunk is # 49, we need to keep references to #47 and #48 as they won't be
			//  iterated over completely (we're clipping through to #49's from) and then add them to the next batch.

		}

		if it.direction == logproto.BACKWARD {
			through = time.Unix(0, headChunk.Chunk.Through.UnixNano())

			if through.After(it.end) {
				through = it.end
			}

			// we have to reverse the inclusivity of the chunk iterator from
			// [from, through) to (from, through] for backward queries, except when
			// the batch's `through` is equal to the query's End. This can be achieved
			// by shifting `through` by one nanosecond.
			if !through.Equal(it.end) {
				through = through.Add(time.Nanosecond)
			}
		} else {

			from = time.Unix(0, headChunk.Chunk.From.UnixNano())

			// if the start of the batch is equal to the end of the query, since the end is not inclusive we can discard that batch.
			if from.Equal(it.end) {
				if it.end != it.start {
					return nil
				}
				// unless end and start are equal in which case start and end are both inclusive.
				from = it.end
			}
			// when clipping the from it should never be before the start.
			// Doing so would include entries not requested.
			if from.Before(it.start) {
				from = it.start
			}
		}

		// it's possible that the current batch and the next batch are fully overlapping in which case
		// we should keep adding more items until the batch boundaries difference is positive.
		if through.Sub(from) > 0 {
			break
		}
	}

	// If every chunk overlaps and we exhaust fetching chunks before ever finding a non overlapping chunk
	// in this case it will be possible to have a through value which is older or equal to our from value
	// If that happens we reset the bounds according to the iteration direction
	if through.Sub(from) <= 0 {
		if it.direction == logproto.BACKWARD {
			from = it.start
		} else {
			through = it.end
		}
	}

	if it.chunks.Len() > 0 {
		it.lastOverlapping = it.lastOverlapping[:0]
		for _, c := range batch {
			if c.IsOverlapping(nextChunk, it.direction) {
				it.lastOverlapping = append(it.lastOverlapping, c)
			}
		}
	}
	// download chunk for this batch.
	chksBySeries, err := fetchChunkBySeries(it.ctx, it.schemas, it.metrics, batch, it.matchers, it.chunkFilterer)
	if err != nil {
		return &chunkBatch{err: err}
	}
	return &chunkBatch{
		chunksBySeries: chksBySeries,
		err:            err,
		from:           from,
		through:        through,
		nextChunk:      nextChunk,
	}
}

type chunkBatch struct {
	chunksBySeries map[model.Fingerprint][][]*LazyChunk
	err            error

	from, through time.Time
	nextChunk     *LazyChunk
}

type logBatchIterator struct {
	*batchChunkIterator
	curr iter.EntryIterator
	err  error

	ctx      context.Context
	cancel   context.CancelFunc
	pipeline syntax.Pipeline
}

func newLogBatchIterator(
	ctx context.Context,
	schemas config.SchemaConfig,
	metrics *ChunkMetrics,
	chunks []*LazyChunk,
	batchSize int,
	matchers []*labels.Matcher,
	pipeline syntax.Pipeline,
	direction logproto.Direction,
	start, end time.Time,
	chunkFilterer chunk.Filterer,
) (iter.EntryIterator, error) {
	ctx, cancel := context.WithCancel(ctx)
	return &logBatchIterator{
		pipeline:           pipeline,
		ctx:                ctx,
		cancel:             cancel,
		batchChunkIterator: newBatchChunkIterator(ctx, schemas, chunks, batchSize, direction, start, end, metrics, matchers, chunkFilterer),
	}, nil
}

func (it *logBatchIterator) Labels() string {
	return it.curr.Labels()
}

func (it *logBatchIterator) StreamHash() uint64 {
	return it.curr.StreamHash()
}

func (it *logBatchIterator) Err() error {
	if it.err != nil {
		return it.err
	}
	if it.curr != nil && it.curr.Err() != nil {
		return it.curr.Err()
	}
	if it.ctx.Err() != nil {
		return it.ctx.Err()
	}
	return nil
}

func (it *logBatchIterator) Close() error {
	it.cancel()
	if it.curr != nil {
		return it.curr.Close()
	}
	return nil
}

func (it *logBatchIterator) At() logproto.Entry {
	return it.curr.At()
}

func (it *logBatchIterator) Next() bool {
	// for loop to avoid recursion
	for {
		if it.curr != nil && it.curr.Next() {
			return true
		}
		// close previous iterator
		if it.curr != nil {
			it.err = it.curr.Close()
		}
		next := it.batchChunkIterator.Next()
		if next == nil {
			return false
		}
		if next.err != nil {
			it.err = next.err
			return false
		}
		var err error
		it.curr, err = it.newChunksIterator(next)
		if err != nil {
			it.err = err
			return false
		}
	}
}

// newChunksIterator creates an iterator over a set of lazychunks.
func (it *logBatchIterator) newChunksIterator(b *chunkBatch) (iter.EntryIterator, error) {
	iters, err := it.buildIterators(b.chunksBySeries, b.from, b.through, b.nextChunk)
	if err != nil {
		return nil, err
	}
	if len(iters) == 1 {
		return iters[0], nil
	}
	return iter.NewSortEntryIterator(iters, it.direction), nil
}

func (it *logBatchIterator) buildIterators(chks map[model.Fingerprint][][]*LazyChunk, from, through time.Time, nextChunk *LazyChunk) ([]iter.EntryIterator, error) {
	result := make([]iter.EntryIterator, 0, len(chks))
	for _, chunks := range chks {
		if len(chunks) != 0 && len(chunks[0]) != 0 {
			streamPipeline := it.pipeline.ForStream(labels.NewBuilder(chunks[0][0].Chunk.Metric).Del(labels.MetricName).Labels())
			iterator, err := it.buildMergeIterator(chunks, from, through, streamPipeline, nextChunk)
			if err != nil {
				return nil, err
			}

			result = append(result, iterator)
		}
	}

	return result, nil
}

func (it *logBatchIterator) buildMergeIterator(chks [][]*LazyChunk, from, through time.Time, streamPipeline log.StreamPipeline, nextChunk *LazyChunk) (iter.EntryIterator, error) {
	result := make([]iter.EntryIterator, 0, len(chks))

	for i := range chks {
		iterators := make([]iter.EntryIterator, 0, len(chks[i]))
		for j := range chks[i] {
			if !chks[i][j].IsValid {
				continue
			}
			iterator, err := chks[i][j].Iterator(it.ctx, from, through, it.direction, streamPipeline, nextChunk)
			if err != nil {
				return nil, err
			}
			iterators = append(iterators, iterator)
		}
		if it.direction == logproto.BACKWARD {
			for i, j := 0, len(iterators)-1; i < j; i, j = i+1, j-1 {
				iterators[i], iterators[j] = iterators[j], iterators[i]
			}
		}
		result = append(result, iter.NewNonOverlappingIterator(iterators))
	}

	return iter.NewMergeEntryIterator(it.ctx, result, it.direction), nil
}

type sampleBatchIterator struct {
	*batchChunkIterator
	curr iter.SampleIterator
	err  error

	ctx        context.Context
	cancel     context.CancelFunc
	extractors []syntax.SampleExtractor
}

func newSampleBatchIterator(
	ctx context.Context,
	schemas config.SchemaConfig,
	metrics *ChunkMetrics,
	chunks []*LazyChunk,
	batchSize int,
	matchers []*labels.Matcher,
	start, end time.Time,
	chunkFilterer chunk.Filterer,
	extractors ...syntax.SampleExtractor,
) (iter.SampleIterator, error) {
	ctx, cancel := context.WithCancel(ctx)
	return &sampleBatchIterator{
		extractors: extractors,
		ctx:        ctx,
		cancel:     cancel,
		batchChunkIterator: newBatchChunkIterator(
			ctx,
			schemas,
			chunks,
			batchSize,
			logproto.FORWARD,
			start,
			end,
			metrics,
			matchers,
			chunkFilterer,
		),
	}, nil
}

func (it *sampleBatchIterator) Labels() string {
	return it.curr.Labels()
}

func (it *sampleBatchIterator) StreamHash() uint64 {
	return it.curr.StreamHash()
}

func (it *sampleBatchIterator) Err() error {
	if it.err != nil {
		return it.err
	}
	if it.curr != nil && it.curr.Err() != nil {
		return it.curr.Err()
	}
	if it.ctx.Err() != nil {
		return it.ctx.Err()
	}
	return nil
}

func (it *sampleBatchIterator) Close() error {
	it.cancel()
	if it.curr != nil {
		return it.curr.Close()
	}
	return nil
}

func (it *sampleBatchIterator) At() logproto.Sample {
	return it.curr.At()
}

func (it *sampleBatchIterator) Next() bool {
	// for loop to avoid recursion
	for it.ctx.Err() == nil {
		if it.curr != nil && it.curr.Next() {
			return true
		}
		// close previous iterator
		if it.curr != nil {
			it.err = it.curr.Close()
		}
		next := it.batchChunkIterator.Next()
		if next == nil {
			return false
		}
		if next.err != nil {
			it.err = next.err
			return false
		}
		var err error
		it.curr, err = it.newChunksIterator(next)
		if err != nil {
			it.err = err
			return false
		}
	}
	return false
}

// newChunksIterator creates an iterator over a set of lazychunks.
func (it *sampleBatchIterator) newChunksIterator(b *chunkBatch) (iter.SampleIterator, error) {
	iters, err := it.buildIterators(b.chunksBySeries, b.from, b.through, b.nextChunk)
	if err != nil {
		return nil, err
	}

	return iter.NewSortSampleIterator(iters), nil
}

func (it *sampleBatchIterator) buildIterators(
	chks map[model.Fingerprint][][]*LazyChunk,
	from, through time.Time,
	nextChunk *LazyChunk,
) ([]iter.SampleIterator, error) {
	result := make([]iter.SampleIterator, 0, len(chks))
	for _, chunks := range chks {
		if len(chunks) != 0 && len(chunks[0]) != 0 {
			extractors := make([]log.StreamSampleExtractor, 0, len(it.extractors))
			for _, extractor := range it.extractors {
				extractors = append(
					extractors,
					extractor.ForStream(
						labels.NewBuilder(chunks[0][0].Chunk.Metric).
							Del(labels.MetricName).
							Labels(),
					),
				)
			}
			iterator, err := it.buildHeapIterator(chunks, from, through, extractors, nextChunk)
			if err != nil {
				return nil, err
			}
			result = append(result, iterator)
		}
	}

	return result, nil
}

func (it *sampleBatchIterator) buildHeapIterator(
	chks [][]*LazyChunk,
	from, through time.Time,
	streamExtractors []log.StreamSampleExtractor,
	nextChunk *LazyChunk,
) (iter.SampleIterator, error) {
	result := make([]iter.SampleIterator, 0, len(chks))

	for i := range chks {
		iterators := make([]iter.SampleIterator, 0, len(chks[i]))
		for j := range chks[i] {
			if !chks[i][j].IsValid {
				continue
			}
			iterator, err := chks[i][j].SampleIterator(it.ctx, from, through, nextChunk, streamExtractors...)
			if err != nil {
				return nil, err
			}
			iterators = append(iterators, iterator)
		}
		result = append(result, iter.NewNonOverlappingSampleIterator(iterators))
	}

	return iter.NewMergeSampleIterator(it.ctx, result), nil
}

func removeMatchersByName(matchers []*labels.Matcher, names ...string) []*labels.Matcher {
	for _, omit := range names {
		for i := range matchers {
			if matchers[i].Name == omit {
				matchers = append(matchers[:i], matchers[i+1:]...)
				break
			}
		}
	}
	return matchers
}

func fetchChunkBySeries(
	ctx context.Context,
	s config.SchemaConfig,
	metrics *ChunkMetrics,
	chunks []*LazyChunk,
	matchers []*labels.Matcher,
	chunkFilter chunk.Filterer,
) (map[model.Fingerprint][][]*LazyChunk, error) {
	chksBySeries := partitionBySeriesChunks(chunks)

	// Make sure the initial chunks are loaded. This is not one chunk
	// per series, but rather a chunk per non-overlapping iterator.
	if err := loadFirstChunks(ctx, s, chksBySeries); err != nil {
		return nil, err
	}

	// Now that we have the first chunk for each series loaded,
	// we can proceed to filter the series that don't match.
	chksBySeries = filterSeriesByMatchers(chksBySeries, matchers, chunkFilter, metrics)

	var allChunks []*LazyChunk
	for _, series := range chksBySeries {
		for _, chunks := range series {
			allChunks = append(allChunks, chunks...)
		}
	}

	// Finally we load all chunks not already loaded
	if err := fetchLazyChunks(ctx, s, allChunks); err != nil {
		return nil, err
	}
	metrics.chunks.WithLabelValues(statusMatched).Add(float64(len(allChunks)))
	metrics.series.WithLabelValues(statusMatched).Add(float64(len(chksBySeries)))
	metrics.batches.WithLabelValues(statusMatched).Observe(float64(len(allChunks)))
	metrics.batches.WithLabelValues(statusDiscarded).Observe(float64(len(chunks) - len(allChunks)))

	return chksBySeries, nil
}

func filterSeriesByMatchers(
	chks map[model.Fingerprint][][]*LazyChunk,
	matchers []*labels.Matcher,
	chunkFilterer chunk.Filterer,
	metrics *ChunkMetrics,
) map[model.Fingerprint][][]*LazyChunk {
	var filteredSeries, filteredChks int

	removeSeries := func(fp model.Fingerprint, chunks [][]*LazyChunk) {
		delete(chks, fp)
		filteredSeries++

		for _, grp := range chunks {
			filteredChks += len(grp)
		}
	}
outer:
	for fp, chunks := range chks {
		for _, matcher := range matchers {
			if !matcher.Matches(chunks[0][0].Chunk.Metric.Get(matcher.Name)) {
				removeSeries(fp, chunks)
				continue outer
			}
		}
		if chunkFilterer != nil && chunkFilterer.ShouldFilter(chunks[0][0].Chunk.Metric) {
			removeSeries(fp, chunks)
			continue outer
		}
	}
	metrics.chunks.WithLabelValues(statusDiscarded).Add(float64(filteredChks))
	metrics.series.WithLabelValues(statusDiscarded).Add(float64(filteredSeries))
	return chks
}

func fetchLazyChunks(ctx context.Context, s config.SchemaConfig, chunks []*LazyChunk) error {
	var (
		totalChunks int64
		start       = time.Now()
		stats       = stats.FromContext(ctx)
		logger      = util_log.WithContext(ctx, util_log.Logger)
	)
	defer func() {
		stats.AddChunksDownloadTime(time.Since(start))
		stats.AddChunksDownloaded(totalChunks)
	}()

	chksByFetcher := map[*fetcher.Fetcher][]*LazyChunk{}
	for _, c := range chunks {
		if c.Chunk.Data == nil {
			chksByFetcher[c.Fetcher] = append(chksByFetcher[c.Fetcher], c)
			totalChunks++
		}
	}
	if len(chksByFetcher) == 0 {
		return nil
	}
	level.Debug(logger).Log("msg", "loading lazy chunks", "chunks", totalChunks)

	errChan := make(chan error)
	for f, chunks := range chksByFetcher {
		go func(fetcher *fetcher.Fetcher, chunks []*LazyChunk) {
			chks := make([]chunk.Chunk, 0, len(chunks))
			index := make(map[string]*LazyChunk, len(chunks))

			for _, chk := range chunks {
				key := s.ExternalKey(chk.Chunk.ChunkRef)
				chks = append(chks, chk.Chunk)
				index[key] = chk
			}
			chks, err := fetcher.FetchChunks(ctx, chks)
			if ctx.Err() != nil {
				errChan <- nil
				return
			}
			if err != nil {
				level.Error(logger).Log("msg", "error fetching chunks", "err", err)
				if isInvalidChunkError(err) {
					level.Error(logger).Log("msg", "checksum of chunks does not match", "err", chunk.ErrInvalidChecksum)
					errChan <- nil
					return
				}
				errChan <- err
				return

			}
			// assign fetched chunk by key as FetchChunks doesn't guarantee the order.
			for _, chk := range chks {
				index[s.ExternalKey(chk.ChunkRef)].Chunk = chk
			}

			errChan <- nil
		}(f, chunks)
	}

	var lastErr error
	for i := 0; i < len(chksByFetcher); i++ {
		if err := <-errChan; err != nil {
			lastErr = err
		}
	}

	if lastErr != nil {
		return lastErr
	}

	for _, c := range chunks {
		if c.Chunk.Data != nil {
			c.IsValid = true
		}
	}
	return nil
}

func isInvalidChunkError(err error) bool {
	err = errors.Cause(err)
	if err, ok := err.(promql.ErrStorage); ok {
		return err.Err == chunk.ErrInvalidChecksum || err.Err == chunkenc.ErrInvalidChecksum
	}
	return false
}

func loadFirstChunks(ctx context.Context, s config.SchemaConfig, chks map[model.Fingerprint][][]*LazyChunk) error {
	var toLoad []*LazyChunk
	for _, lchks := range chks {
		for _, lchk := range lchks {
			if len(lchk) == 0 {
				continue
			}
			toLoad = append(toLoad, lchk[0])
		}
	}
	return fetchLazyChunks(ctx, s, toLoad)
}

func partitionBySeriesChunks(chunks []*LazyChunk) map[model.Fingerprint][][]*LazyChunk {
	chunksByFp := map[model.Fingerprint][]*LazyChunk{}
	for _, c := range chunks {
		fp := c.Chunk.FingerprintModel()
		chunksByFp[fp] = append(chunksByFp[fp], c)
	}
	result := make(map[model.Fingerprint][][]*LazyChunk, len(chunksByFp))

	for fp, chks := range chunksByFp {
		result[fp] = partitionOverlappingChunks(chks)
	}

	return result
}

// partitionOverlappingChunks splits the list of chunks into different non-overlapping lists.
// todo this might reverse the order.
func partitionOverlappingChunks(chunks []*LazyChunk) [][]*LazyChunk {
	sort.Slice(chunks, func(i, j int) bool {
		return chunks[i].Chunk.From < chunks[j].Chunk.From
	})

	css := [][]*LazyChunk{}
outer:
	for _, c := range chunks {
		for i, cs := range css {
			// If the chunk doesn't overlap with the current list, then add it to it.
			if cs[len(cs)-1].Chunk.Through.Before(c.Chunk.From) {
				css[i] = append(css[i], c)
				continue outer
			}
		}
		// If the chunk overlaps with every existing list, then create a new list.
		cs := make([]*LazyChunk, 0, len(chunks)/(len(css)+1))
		cs = append(cs, c)
		css = append(css, cs)
	}

	return css
}
