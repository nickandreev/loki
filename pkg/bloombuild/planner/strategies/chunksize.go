package strategies

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/dustin/go-humanize"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/grafana/loki/v3/pkg/bloombuild/protos"
	iter "github.com/grafana/loki/v3/pkg/iter/v2"
	v1 "github.com/grafana/loki/v3/pkg/storage/bloom/v1"
	"github.com/grafana/loki/v3/pkg/storage/config"
	"github.com/grafana/loki/v3/pkg/storage/stores/shipper/bloomshipper"
	"github.com/grafana/loki/v3/pkg/storage/stores/shipper/indexshipper/tsdb"
	"github.com/grafana/loki/v3/pkg/storage/stores/shipper/indexshipper/tsdb/index"
)

type ChunkSizeStrategyLimits interface {
	BloomTaskTargetSeriesChunksSizeBytes(tenantID string) uint64
}

type ChunkSizeStrategy struct {
	limits ChunkSizeStrategyLimits
	logger log.Logger
}

func NewChunkSizeStrategy(
	limits ChunkSizeStrategyLimits,
	logger log.Logger,
) (*ChunkSizeStrategy, error) {
	return &ChunkSizeStrategy{
		limits: limits,
		logger: logger,
	}, nil
}

func (s *ChunkSizeStrategy) Name() string {
	return SplitBySeriesChunkSizeStrategyName
}

func (s *ChunkSizeStrategy) Plan(
	ctx context.Context,
	table config.DayTable,
	tenant string,
	tsdbs TSDBSet,
	metas []bloomshipper.Meta,
) ([]*protos.Task, error) {
	targetTaskSize := s.limits.BloomTaskTargetSeriesChunksSizeBytes(tenant)

	logger := log.With(s.logger, "table", table.Addr(), "tenant", tenant)
	level.Debug(logger).Log("msg", "loading work for tenant", "target task size", humanize.Bytes(targetTaskSize))

	// Determine which TSDBs have gaps and need to be processed.
	tsdbsWithGaps, err := gapsBetweenTSDBsAndMetas(v1.NewBounds(0, math.MaxUint64), tsdbs, metas)
	if err != nil {
		level.Error(logger).Log("msg", "failed to find gaps", "err", err)
		return nil, fmt.Errorf("failed to find gaps: %w", err)
	}

	if len(tsdbsWithGaps) == 0 {
		level.Debug(logger).Log("msg", "blooms exist for all tsdbs")
		return nil, nil
	}

	sizedIter, iterSize, err := s.sizedSeriesIter(ctx, tenant, tsdbsWithGaps, targetTaskSize)
	if err != nil {
		return nil, fmt.Errorf("failed to get sized series iter: %w", err)
	}

	tasks := make([]*protos.Task, 0, iterSize)
	for sizedIter.Next() {
		series := sizedIter.At()
		if series.Len() == 0 {
			// This should never happen, but just in case.
			level.Warn(logger).Log("msg", "got empty series batch", "tsdb", series.TSDB().Name())
			continue
		}

		bounds := series.Bounds()

		blocks, err := getBlocksMatchingBounds(metas, bounds)
		if err != nil {
			return nil, fmt.Errorf("failed to get blocks matching bounds: %w", err)
		}

		planGap := protos.Gap{
			Bounds: bounds,
			Series: series.V1Series(),
			Blocks: blocks,
		}

		tasks = append(tasks, protos.NewTask(table, tenant, bounds, series.TSDB(), []protos.Gap{planGap}))
	}
	if err := sizedIter.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate over sized series: %w", err)
	}

	return tasks, nil
}

func getBlocksMatchingBounds(metas []bloomshipper.Meta, bounds v1.FingerprintBounds) ([]bloomshipper.BlockRef, error) {
	blocks := make([]bloomshipper.BlockRef, 0, 10)

	for _, meta := range metas {
		if meta.Bounds.Intersection(bounds) == nil {
			// this meta doesn't overlap the gap, skip
			continue
		}

		for _, block := range meta.Blocks {
			if block.Bounds.Intersection(bounds) == nil {
				// this block doesn't overlap the gap, skip
				continue
			}
			// this block overlaps the gap, add it to the plan
			// for this gap
			blocks = append(blocks, block)
		}
	}

	// ensure we sort blocks so deduping iterator works as expected
	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i].Bounds.Less(blocks[j].Bounds)
	})

	peekingBlocks := iter.NewPeekIter(
		iter.NewSliceIter(
			blocks,
		),
	)

	// dedupe blocks which could be in multiple metas
	itr := iter.NewDedupingIter(
		func(a, b bloomshipper.BlockRef) bool {
			return a == b
		},
		iter.Identity[bloomshipper.BlockRef],
		func(a, _ bloomshipper.BlockRef) bloomshipper.BlockRef {
			return a
		},
		peekingBlocks,
	)

	deduped, err := iter.Collect(itr)
	if err != nil {
		return nil, fmt.Errorf("failed to dedupe blocks: %w", err)
	}

	return deduped, nil
}

type seriesBatch struct {
	tsdb   tsdb.SingleTenantTSDBIdentifier
	series []*v1.Series
	size   uint64
}

func newSeriesBatch(tsdb tsdb.SingleTenantTSDBIdentifier) seriesBatch {
	return seriesBatch{
		tsdb:   tsdb,
		series: make([]*v1.Series, 0, 100),
	}
}

func (b *seriesBatch) Bounds() v1.FingerprintBounds {
	if len(b.series) == 0 {
		return v1.NewBounds(0, 0)
	}

	// We assume that the series are sorted by fingerprint.
	// This is guaranteed since series are iterated in order by the TSDB.
	return v1.NewBounds(b.series[0].Fingerprint, b.series[len(b.series)-1].Fingerprint)
}

func (b *seriesBatch) V1Series() []*v1.Series {
	return b.series
}

func (b *seriesBatch) Append(s *v1.Series, size uint64) {
	b.series = append(b.series, s)
	b.size += size
}

func (b *seriesBatch) Len() int {
	return len(b.series)
}

func (b *seriesBatch) Size() uint64 {
	return b.size
}

func (b *seriesBatch) TSDB() tsdb.SingleTenantTSDBIdentifier {
	return b.tsdb
}

func (s *ChunkSizeStrategy) sizedSeriesIter(
	ctx context.Context,
	tenant string,
	tsdbsWithGaps []tsdbGaps,
	targetTaskSizeBytes uint64,
) (iter.Iterator[seriesBatch], int, error) {
	batches := make([]seriesBatch, 0, 100)
	var currentBatch seriesBatch

	for _, idx := range tsdbsWithGaps {
		if currentBatch.Len() > 0 {
			batches = append(batches, currentBatch)
		}
		currentBatch = newSeriesBatch(idx.tsdbIdentifier)

		for _, gap := range idx.gaps {
			if err := idx.tsdb.ForSeries(
				ctx,
				tenant,
				gap,
				0, math.MaxInt64,
				func(_ labels.Labels, fp model.Fingerprint, chks []index.ChunkMeta) (stop bool) {
					select {
					case <-ctx.Done():
						return true
					default:
						var seriesSize uint64
						for _, chk := range chks {
							seriesSize += uint64(chk.KB * 1024)
						}

						// Cut a new batch IF the current batch is not empty (so we add at least one series to the batch)
						// AND Adding this series to the batch would exceed the target task size.
						if currentBatch.Len() > 0 && currentBatch.Size()+seriesSize > targetTaskSizeBytes {
							batches = append(batches, currentBatch)
							currentBatch = newSeriesBatch(idx.tsdbIdentifier)
						}

						res := &v1.Series{
							Fingerprint: fp,
							Chunks:      make(v1.ChunkRefs, 0, len(chks)),
						}
						for _, chk := range chks {
							res.Chunks = append(res.Chunks, v1.ChunkRef{
								From:     model.Time(chk.MinTime),
								Through:  model.Time(chk.MaxTime),
								Checksum: chk.Checksum,
							})
						}

						currentBatch.Append(res, seriesSize)
						return false
					}
				},
				labels.MustNewMatcher(labels.MatchEqual, "", ""),
			); err != nil {
				return nil, 0, err
			}

			// Add the last batch for this gap if it's not empty.
			if currentBatch.Len() > 0 {
				batches = append(batches, currentBatch)
				currentBatch = newSeriesBatch(idx.tsdbIdentifier)
			}
		}
	}

	select {
	case <-ctx.Done():
		return iter.NewEmptyIter[seriesBatch](), 0, ctx.Err()
	default:
		return iter.NewCancelableIter[seriesBatch](ctx, iter.NewSliceIter[seriesBatch](batches)), len(batches), nil
	}
}
