// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package internal // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/internal"

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

const (
	// language=ClickHouse SQL
	createSumTableSQL = `
CREATE TABLE IF NOT EXISTS %s_sum ON CLUSTER '{cluster}' (
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ResourceSchemaUrl String CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion String CODEC(ZSTD(1)),
    ScopeAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeDroppedAttrCount UInt32 CODEC(ZSTD(1)),
    ScopeSchemaUrl String CODEC(ZSTD(1)),
    MetricName String CODEC(ZSTD(1)),
    MetricDescription String CODEC(ZSTD(1)),
    MetricUnit String CODEC(ZSTD(1)),
    Attributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
	StartTimeUnix DateTime64(9) CODEC(Delta, ZSTD(1)),
	TimeUnix DateTime64(9) CODEC(Delta, ZSTD(1)),
	Value Float64 CODEC(ZSTD(1)),
	Flags UInt32  CODEC(ZSTD(1)),
    Exemplars Nested (
		FilteredAttributes Map(LowCardinality(String), String),
		TimeUnix DateTime64(9),
		Value Float64,
		SpanId String,
		TraceId String
    ) CODEC(ZSTD(1)),
    AggTemp Int32 CODEC(ZSTD(1)),
	IsMonotonic Boolean CODEC(Delta, ZSTD(1)),
	INDEX idx_res_attr_key mapKeys(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
	INDEX idx_res_attr_value mapValues(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
	INDEX idx_scope_attr_key mapKeys(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
	INDEX idx_scope_attr_value mapValues(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
	INDEX idx_attr_key mapKeys(Attributes) TYPE bloom_filter(0.01) GRANULARITY 1,
	INDEX idx_attr_value mapValues(Attributes) TYPE bloom_filter(0.01) GRANULARITY 1
) ENGINE MergeTree()
%s
PARTITION BY toDate(TimeUnix)
ORDER BY (MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix))
SETTINGS index_granularity=8192, ttl_only_drop_parts = 1;
`
	// language=ClickHouse SQL
	insertSumTableSQL = `INSERT INTO %s_sum (
    ResourceAttributes,
    ResourceSchemaUrl,
    ScopeName,
    ScopeVersion,
    ScopeAttributes,
	ScopeDroppedAttrCount,
    ScopeSchemaUrl,
    MetricName,
    MetricDescription,
    MetricUnit,
    Attributes,
    StartTimeUnix,
    TimeUnix,
    Value,
    Flags,
    Exemplars.FilteredAttributes,
	Exemplars.TimeUnix,
    Exemplars.Value,
    Exemplars.SpanId,
    Exemplars.TraceId,
	AggTemp,
	IsMonotonic) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
)

type sumModel struct {
	metricName        string
	metricDescription string
	metricUnit        string
	metadata          *MetricsMetaData
	sum               pmetric.Sum
}

type sumMetrics struct {
	sumModel  []*sumModel
	insertSQL string
	count     int
}

func (s *sumMetrics) insert(ctx context.Context, db driver.Conn) error {
	if s.count == 0 {
		return nil
	}
	start := time.Now()
	err := func() error {
		var err error
		for _, model := range s.sumModel {
			for i := 0; i < model.sum.DataPoints().Len(); i++ {
				dp := model.sum.DataPoints().At(i)
				attrs, times, values, traceIDs, spanIDs := convertExemplars(dp.Exemplars())
				err = db.AsyncInsert(ctx, s.insertSQL, false,
					model.metadata.ResAttr,
					model.metadata.ResURL,
					model.metadata.ScopeInstr.Name(),
					model.metadata.ScopeInstr.Version(),
					attributesToMap(model.metadata.ScopeInstr.Attributes()),
					model.metadata.ScopeInstr.DroppedAttributesCount(),
					model.metadata.ScopeURL,
					model.metricName,
					model.metricDescription,
					model.metricUnit,
					attributesToMap(dp.Attributes()),
					dp.StartTimestamp().AsTime(),
					dp.Timestamp().AsTime(),
					getValue(dp.IntValue(), dp.DoubleValue(), dp.ValueType()),
					uint32(dp.Flags()),
					attrs,
					times,
					values,
					spanIDs,
					traceIDs,
					int32(model.sum.AggregationTemporality()),
					model.sum.IsMonotonic(),
				)
				if err != nil {
					return fmt.Errorf("ExecContext:%w", err)
				}
			}
		}
		return err
	}()
	duration := time.Since(start)
	if err != nil {
		logger.Debug("insert sum metrics fail", zap.Duration("cost", duration))
		return fmt.Errorf("insert sum metrics fail:%w", err)
	}

	// TODO latency metrics
	logger.Debug("insert sum metrics", zap.Int("records", s.count),
		zap.Duration("cost", duration))
	return nil
}

func (s *sumMetrics) Add(resAttr map[string]string, resURL string, scopeInstr pcommon.InstrumentationScope, scopeURL string, metrics any, name string, description string, unit string) error {
	sum, ok := metrics.(pmetric.Sum)
	if !ok {
		return fmt.Errorf("metrics param is not type of Sum")
	}
	s.count += sum.DataPoints().Len()
	s.sumModel = append(s.sumModel, &sumModel{
		metricName:        name,
		metricDescription: description,
		metricUnit:        unit,
		metadata: &MetricsMetaData{
			ResAttr:    resAttr,
			ResURL:     resURL,
			ScopeURL:   scopeURL,
			ScopeInstr: scopeInstr,
		},
		sum: sum,
	})
	return nil
}
