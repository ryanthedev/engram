package harvester

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
)

// RunOptions defines options for running the harvester orchestration.
type RunOptions struct {
	Collections []string
	Sources     []string
	BatchSize   int
	Logger      *slog.Logger
}

// Run executes the harvesting process. It validates the manifest, then for each
// collection and source, it runs the harvest. Per-source errors are aggregated.
func Run(ctx context.Context, ec EngramClient, m Manifest, opts RunOptions) (int, int, error) {
	if err := m.Validate(ctx, ec); err != nil {
		return 0, 0, fmt.Errorf("harvester: validating manifest: %w", err)
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	filterCols := make(map[string]bool)
	for _, c := range opts.Collections {
		filterCols[c] = true
	}

	filterSrcs := make(map[string]bool)
	for _, s := range opts.Sources {
		filterSrcs[s] = true
	}

	var totalIndexed int
	var totalDeleted int
	var errs []error

	for _, col := range m.Collections {
		if len(filterCols) > 0 && !filterCols[col.Name] {
			continue
		}

		for _, srcCfg := range col.Sources {
			if len(filterSrcs) > 0 && !filterSrcs[srcCfg.Type] {
				continue
			}

			logger.InfoContext(ctx, "harvester: starting source run",
				slog.String("collection", col.Name),
				slog.String("source_type", srcCfg.Type),
			)

			src, err := Build(srcCfg, Deps{Logger: logger})
			if err != nil {
				wrappedErr := fmt.Errorf("harvester: building source for collection %q, type %q: %w", col.Name, srcCfg.Type, err)
				errs = append(errs, wrappedErr)
				logger.ErrorContext(ctx, "harvester: failed to build source",
					slog.String("collection", col.Name),
					slog.String("source_type", srcCfg.Type),
					slog.Any("error", err),
				)
				continue
			}

			runner := NewRunner(ec, opts.BatchSize, logger)
			report, err := runner.Run(ctx, col.Name, src)
			if err != nil {
				wrappedErr := fmt.Errorf("harvester: running source for collection %q, type %q: %w", col.Name, srcCfg.Type, err)
				errs = append(errs, wrappedErr)
				logger.ErrorContext(ctx, "harvester: source run failed",
					slog.String("collection", col.Name),
					slog.String("source_type", srcCfg.Type),
					slog.Any("error", err),
				)
				continue
			}

			totalIndexed += report.Indexed
			totalDeleted += report.Deleted

			logger.InfoContext(ctx, "harvester: source run finished",
				slog.String("collection", col.Name),
				slog.String("source_type", srcCfg.Type),
				slog.Int("indexed", report.Indexed),
				slog.Int("deleted", report.Deleted),
			)
		}
	}

	var aggErr error
	if len(errs) > 0 {
		aggErr = errors.Join(errs...)
	}

	return totalIndexed, totalDeleted, aggErr
}

// ValidateFilters ensures that the specified filter collections and sources exist in the manifest.
func ValidateFilters(m Manifest, filterCollections []string, filterSources []string) error {
	manifestCols := make(map[string]bool)
	manifestSrcs := make(map[string]bool)
	for _, col := range m.Collections {
		manifestCols[col.Name] = true
		for _, src := range col.Sources {
			manifestSrcs[src.Type] = true
		}
	}

	if len(filterCollections) > 0 {
		var invalidCols []string
		for _, colName := range filterCollections {
			if !manifestCols[colName] {
				invalidCols = append(invalidCols, colName)
			}
		}
		if len(invalidCols) > 0 {
			var validCols []string
			for c := range manifestCols {
				validCols = append(validCols, c)
			}
			sort.Strings(validCols)
			return fmt.Errorf("harvester: unknown collection(s) %v; valid options: %v", invalidCols, validCols)
		}
	}

	if len(filterSources) > 0 {
		var invalidSrcs []string
		for _, srcType := range filterSources {
			if !manifestSrcs[srcType] {
				invalidSrcs = append(invalidSrcs, srcType)
			}
		}
		if len(invalidSrcs) > 0 {
			var validSrcs []string
			for s := range manifestSrcs {
				validSrcs = append(validSrcs, s)
			}
			sort.Strings(validSrcs)
			return fmt.Errorf("harvester: unknown source(s) %v; valid options: %v", invalidSrcs, validSrcs)
		}
	}

	return nil
}
