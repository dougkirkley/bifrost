package logstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RelayLog records one call to a relay (/relay/{endpoint}/{path}),
// or the settlement of an async job one started. It is a table of its own, as MCP tool and Agent
// Gateway logs are, because a relay call has no provider or model: its cost is the
// endpoint's pricing formula, recorded here broken down.
type RelayLog struct {
	ID        string    `gorm:"primaryKey;type:varchar(255)" json:"id"`
	Timestamp time.Time `gorm:"index:idx_relay_logs_timestamp;not null" json:"timestamp"`
	CreatedAt time.Time `gorm:"index:idx_relay_logs_created_at;not null;autoCreateTime:false;default:CURRENT_TIMESTAMP" json:"created_at"`

	RelayName  string `gorm:"type:varchar(64);index:idx_relay_logs_relay_name;not null" json:"relay_name"`
	Kind       string `gorm:"type:varchar(16);index:idx_relay_logs_kind;not null;default:'call'" json:"kind"` // call | job
	Method     string `gorm:"type:varchar(10);index:idx_relay_logs_method" json:"method"`
	Path       string `gorm:"type:text" json:"path"`
	RoutePath  string `gorm:"type:text" json:"route_path,omitempty"`                               // matched route glob, "" for none
	Status     string `gorm:"type:varchar(32);index:idx_relay_logs_status;not null" json:"status"` // success | error | cancelled
	StatusCode int    `json:"status_code"`
	Streamed   bool   `json:"streamed"`
	Billable   bool   `json:"billable"`
	Model      string `gorm:"type:varchar(255)" json:"model,omitempty"`

	Latency  *float64 `json:"latency,omitempty"` // milliseconds
	BytesOut int64    `json:"bytes_out"`

	// Cost is the figure charged to budgets; the rest explains it.
	Cost           *float64 `gorm:"index:idx_relay_logs_cost" json:"cost,omitempty"`
	CostPerRequest *float64 `json:"cost_per_request,omitempty"`
	CostPerUnit    *float64 `json:"cost_per_unit,omitempty"`
	ExtractedCost  *float64 `json:"extracted_cost,omitempty"`
	Units          *float64 `json:"units,omitempty"`
	BilledUnits    *float64 `json:"billed_units,omitempty"`
	InputTokens    int      `json:"input_tokens,omitempty"`
	OutputTokens   int      `json:"output_tokens,omitempty"`
	TotalTokens    int      `json:"total_tokens,omitempty"`

	JobID       *string `gorm:"type:varchar(255);index:idx_relay_logs_job_id" json:"job_id,omitempty"`
	JobStatus   *string `gorm:"type:varchar(64)" json:"job_status,omitempty"`
	ParentLogID *string `gorm:"type:varchar(255);index:idx_relay_logs_parent_log_id" json:"parent_log_id,omitempty"` // a job row's start call

	UserID                *string               `gorm:"type:varchar(255);index:idx_relay_logs_user_id" json:"user_id,omitempty"`
	UserName              *string               `gorm:"type:varchar(255)" json:"user_name,omitempty"`
	VirtualKeyID          *string               `gorm:"type:varchar(255);index:idx_relay_logs_virtual_key_id" json:"virtual_key_id,omitempty"`
	VirtualKeyName        *string               `gorm:"type:varchar(255)" json:"virtual_key_name,omitempty"`
	TeamID                *string               `gorm:"type:varchar(255);index:idx_relay_logs_team_id" json:"team_id,omitempty"`
	TeamName              *string               `gorm:"type:varchar(255)" json:"team_name,omitempty"`
	CustomerID            *string               `gorm:"type:varchar(255);index:idx_relay_logs_customer_id" json:"customer_id,omitempty"`
	CustomerName          *string               `gorm:"type:varchar(255)" json:"customer_name,omitempty"`
	BusinessUnitID        *string               `gorm:"type:varchar(255);index:idx_relay_logs_business_unit_id" json:"business_unit_id,omitempty"`
	BusinessUnitName      *string               `gorm:"type:varchar(255)" json:"business_unit_name,omitempty"`
	ProjectID             *string               `gorm:"type:varchar(255);index:idx_relay_logs_project_id" json:"project_id,omitempty"`
	ProjectName           *string               `gorm:"type:varchar(255)" json:"project_name,omitempty"`
	BudgetIDs             *string               `gorm:"type:text" json:"-"`
	RateLimitIDs          *string               `gorm:"type:text" json:"-"`
	RequestID             string                `gorm:"type:varchar(255);index:idx_relay_logs_request_id;not null" json:"request_id"`
	RequestHeaders        *string               `gorm:"type:text" json:"-"`
	ResponseHeaders       *string               `gorm:"type:text" json:"-"`
	RequestBody           *string               `gorm:"type:text" json:"request_body,omitempty"`
	ResponseBody          *string               `gorm:"type:text" json:"response_body,omitempty"`
	ErrorDetails          string                `gorm:"type:text" json:"-"`
	ContentHidden         bool                  `gorm:"default:false" json:"content_hidden"`
	PluginLogs            string                `gorm:"type:text" json:"plugin_logs,omitempty"`
	BudgetIDsParsed       []string              `gorm:"-" json:"budget_ids,omitempty"`
	RateLimitIDsParsed    []string              `gorm:"-" json:"rate_limit_ids,omitempty"`
	RequestHeadersParsed  map[string]string     `gorm:"-" json:"request_headers,omitempty"`
	ResponseHeadersParsed map[string]string     `gorm:"-" json:"response_headers,omitempty"`
	ErrorDetailsParsed    *schemas.BifrostError `gorm:"-" json:"error_details,omitempty"`
}

// Relay log kinds and statuses.
const (
	RelayLogKindCall = "call"
	RelayLogKindJob  = "job"

	RelayLogStatusSuccess   = "success"
	RelayLogStatusError     = "error"
	RelayLogStatusCancelled = "cancelled"
)

func (RelayLog) TableName() string { return "relay_logs" }

func (l *RelayLog) BeforeCreate(_ *gorm.DB) error {
	if l.Timestamp.IsZero() {
		l.Timestamp = time.Now().UTC()
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = l.Timestamp
	}
	if l.Kind == "" {
		l.Kind = RelayLogKindCall
	}
	return l.SerializeFields()
}

func (l *RelayLog) AfterFind(_ *gorm.DB) error {
	return l.DeserializeFields()
}

// SerializeFields writes the parsed fields into their stored columns.
func (l *RelayLog) SerializeFields() error {
	marshal := func(v any, stored **string) error {
		data, err := sonic.Marshal(v)
		if err != nil {
			return err
		}
		value := string(data)
		*stored = &value
		return nil
	}
	if len(l.BudgetIDsParsed) > 0 {
		if err := marshal(l.BudgetIDsParsed, &l.BudgetIDs); err != nil {
			return fmt.Errorf("failed to serialize relay budget ids: %w", err)
		}
	}
	if len(l.RateLimitIDsParsed) > 0 {
		if err := marshal(l.RateLimitIDsParsed, &l.RateLimitIDs); err != nil {
			return fmt.Errorf("failed to serialize relay rate limit ids: %w", err)
		}
	}
	if len(l.RequestHeadersParsed) > 0 {
		if err := marshal(l.RequestHeadersParsed, &l.RequestHeaders); err != nil {
			return fmt.Errorf("failed to serialize relay request headers: %w", err)
		}
	}
	if len(l.ResponseHeadersParsed) > 0 {
		if err := marshal(l.ResponseHeadersParsed, &l.ResponseHeaders); err != nil {
			return fmt.Errorf("failed to serialize relay response headers: %w", err)
		}
	}
	if l.ErrorDetailsParsed != nil {
		data, err := sonic.Marshal(l.ErrorDetailsParsed)
		if err != nil {
			return fmt.Errorf("failed to serialize relay error details: %w", err)
		}
		l.ErrorDetails = string(data)
	}
	return nil
}

// DeserializeFields reads the stored columns back into their parsed fields. A malformed column is
// left unparsed rather than failing the read.
func (l *RelayLog) DeserializeFields() error {
	unmarshal := func(stored *string, v any) {
		if stored != nil && *stored != "" {
			_ = sonic.Unmarshal([]byte(*stored), v)
		}
	}
	unmarshal(l.BudgetIDs, &l.BudgetIDsParsed)
	unmarshal(l.RateLimitIDs, &l.RateLimitIDsParsed)
	unmarshal(l.RequestHeaders, &l.RequestHeadersParsed)
	unmarshal(l.ResponseHeaders, &l.ResponseHeadersParsed)
	l.ErrorDetailsParsed = nil
	if l.ErrorDetails != "" {
		var details schemas.BifrostError
		if err := sonic.Unmarshal([]byte(l.ErrorDetails), &details); err == nil {
			l.ErrorDetailsParsed = &details
		}
	}
	return nil
}

// RelayLogFilters narrows a Relay Logs search. Slice fields match any of their
// values; every populated field must match.
type RelayLogFilters struct {
	EndpointNames   []string   `json:"relay_names,omitempty"`
	Methods         []string   `json:"methods,omitempty"`
	Statuses        []string   `json:"statuses,omitempty"`
	Kinds           []string   `json:"kinds,omitempty"`
	VirtualKeyIDs   []string   `json:"virtual_key_ids,omitempty"`
	UserIDs         []string   `json:"user_ids,omitempty"`
	TeamIDs         []string   `json:"team_ids,omitempty"`
	CustomerIDs     []string   `json:"customer_ids,omitempty"`
	BusinessUnitIDs []string   `json:"business_unit_ids,omitempty"`
	ProjectIDs      []string   `json:"project_ids,omitempty"`
	Billable        *bool      `json:"billable,omitempty"`
	StartTime       *time.Time `json:"start_time,omitempty"`
	EndTime         *time.Time `json:"end_time,omitempty"`
	MinCost         *float64   `json:"min_cost,omitempty"`
	MaxCost         *float64   `json:"max_cost,omitempty"`
	RequestID       string     `json:"request_id,omitempty"`
	JobID           string     `json:"job_id,omitempty"`
	// Search matches the path, route, model and request id: metadata columns only, never bodies.
	Search string `json:"search,omitempty"`
}

// RelayLogSearchResult is one page of a Relay Logs search.
type RelayLogSearchResult struct {
	Logs       []*RelayLog       `json:"logs"`
	Pagination PaginationOptions `json:"pagination"`
	HasLogs    bool              `json:"has_logs"`
}

// RelayLogStats aggregates a Relay Logs search.
type RelayLogStats struct {
	TotalRequests  int64   `json:"total_requests"`
	SuccessCount   int64   `json:"success_count"`
	ErrorCount     int64   `json:"error_count"`
	SuccessRate    float64 `json:"success_rate"`    // percentage of requests that succeeded
	AverageLatency float64 `json:"average_latency"` // milliseconds
	TotalCost      float64 `json:"total_cost"`
	TotalUnits     float64 `json:"total_units"`
}

// RelayHistogramBucket is one time bucket of volume and spend.
type RelayHistogramBucket struct {
	Timestamp time.Time `json:"timestamp"`
	Count     int64     `json:"count"`
	Success   int64     `json:"success"`
	Error     int64     `json:"error"`
	Cancelled int64     `json:"cancelled"` // calls the caller hung up on, as in the LLM logs histogram
	Cost      float64   `json:"cost"`
}

// RelayHistogramResult is a time-bucketed Relay Logs histogram.
type RelayHistogramResult struct {
	Buckets           []RelayHistogramBucket `json:"buckets"`
	BucketSizeSeconds int64                  `json:"bucket_size_seconds"`
}

// RelayTopResult is one endpoint's totals in a top-endpoints ranking.
type RelayTopResult struct {
	RelayName      string  `json:"relay_name"`
	Count          int64   `json:"count"`
	ErrorCount     int64   `json:"error_count"`
	Cost           float64 `json:"cost"`
	AverageLatency float64 `json:"average_latency"`
}

// RelayTopResults ranks endpoints by request count.
type RelayTopResults struct {
	Endpoints []RelayTopResult `json:"endpoints"`
}

// RelayFilterData lists the values the Relay Logs filters can offer.
type RelayFilterData struct {
	EndpointNames []string            `json:"relay_names"`
	Methods       []string            `json:"methods"`
	VirtualKeys   []RelayFilterOption `json:"virtual_keys"`
}

// RelayFilterOption is an id with the name recorded for it.
type RelayFilterOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// relayLogSortColumns maps the sortable fields to their columns.
var relayLogSortColumns = map[string]string{
	"timestamp": "timestamp",
	"latency":   "latency",
	"cost":      "cost",
}

func (s *RDBLogStore) applyRelayLogFilters(query *gorm.DB, f RelayLogFilters) *gorm.DB {
	for column, values := range map[string][]string{
		"relay_name": f.EndpointNames, "method": f.Methods, "status": f.Statuses, "kind": f.Kinds,
		"virtual_key_id": f.VirtualKeyIDs, "user_id": f.UserIDs, "team_id": f.TeamIDs,
		"customer_id": f.CustomerIDs, "business_unit_id": f.BusinessUnitIDs, "project_id": f.ProjectIDs,
	} {
		if len(values) > 0 {
			query = query.Where(column+" IN ?", values)
		}
	}
	if f.Billable != nil {
		query = query.Where("billable = ?", *f.Billable)
	}
	// timestampBound keeps sub-second bounds on ClickHouse, whose driver drops them otherwise.
	if f.StartTime != nil {
		placeholder, arg := timestampBound(s.db.Dialector.Name(), *f.StartTime, lowerTimeBound)
		query = query.Where("timestamp >= "+placeholder, arg)
	}
	if f.EndTime != nil {
		placeholder, arg := timestampBound(s.db.Dialector.Name(), *f.EndTime, upperTimeBound)
		query = query.Where("timestamp <= "+placeholder, arg)
	}
	if f.MinCost != nil {
		query = query.Where("cost >= ?", *f.MinCost)
	}
	if f.MaxCost != nil {
		query = query.Where("cost <= ?", *f.MaxCost)
	}
	if f.RequestID != "" {
		query = query.Where("request_id = ?", f.RequestID)
	}
	if f.JobID != "" {
		query = query.Where("job_id = ?", f.JobID)
	}
	if search := strings.TrimSpace(f.Search); search != "" {
		like := "%" + escapeLike(search) + "%"
		// Backslash is the default LIKE escape on Postgres, MySQL and ClickHouse, and ClickHouse
		// rejects an ESCAPE clause; only SQLite needs it declared.
		escape := ""
		if query.Dialector != nil && query.Dialector.Name() == "sqlite" {
			escape = ` ESCAPE '\'`
		}
		query = query.Where("(path LIKE ?"+escape+" OR route_path LIKE ?"+escape+" OR model LIKE ?"+escape+" OR request_id LIKE ?"+escape+")", like, like, like, like)
	}
	return query
}

// escapeLike escapes the LIKE wildcards in a search term.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// BatchCreateRelayLogsIfNotExists inserts the rows whose id is not stored yet and
// returns the ids it inserted.
func (s *RDBLogStore) BatchCreateRelayLogsIfNotExists(ctx context.Context, entries []*RelayLog) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	inserted := make([]string, 0, len(entries))
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, entry := range entries {
			if entry == nil {
				continue
			}
			result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(entry)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 1 {
				inserted = append(inserted, entry.ID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inserted, nil
}

// FindRelayLog loads one row, ErrNotFound when it does not exist or is out of scope.
func (s *RDBLogStore) FindRelayLog(ctx context.Context, id string) (*RelayLog, error) {
	var entry RelayLog
	if err := s.ScopedDB(ctx).Where("id = ?", id).First(&entry).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &entry, nil
}

// SearchRelayLogs returns one page of rows matching the filters, newest first by default.
func (s *RDBLogStore) SearchRelayLogs(ctx context.Context, filters RelayLogFilters, pagination PaginationOptions) (*RelayLogSearchResult, error) {
	query := s.applyRelayLogFilters(s.ScopedDB(ctx).Model(&RelayLog{}), filters)
	if !pagination.SkipCount {
		if err := query.Session(&gorm.Session{}).Count(&pagination.TotalCount).Error; err != nil {
			return nil, fmt.Errorf("failed to count relay logs: %w", err)
		}
	}
	column, ok := relayLogSortColumns[pagination.SortBy]
	if !ok {
		column = "timestamp"
	}
	direction := "DESC"
	if strings.EqualFold(pagination.Order, "asc") {
		direction = "ASC"
	}
	if pagination.Limit <= 0 || pagination.Limit > 1000 {
		pagination.Limit = 50
	}
	var logs []*RelayLog
	if err := query.Order(column + " " + direction).Order("id " + direction).Limit(pagination.Limit).Offset(pagination.Offset).Find(&logs).Error; err != nil {
		return nil, fmt.Errorf("failed to search relay logs: %w", err)
	}
	hasLogs := len(logs) > 0
	if !hasLogs {
		var probe []string
		if err := s.ScopedDB(ctx).Model(&RelayLog{}).Limit(1).Pluck("id", &probe).Error; err == nil {
			hasLogs = len(probe) > 0
		}
	}
	return &RelayLogSearchResult{Logs: logs, Pagination: pagination, HasLogs: hasLogs}, nil
}

// GetRelayLogStats aggregates the rows matching the filters.
func (s *RDBLogStore) GetRelayLogStats(ctx context.Context, filters RelayLogFilters) (*RelayLogStats, error) {
	query := s.applyRelayLogFilters(s.ScopedDB(ctx).Model(&RelayLog{}), filters)
	var row struct {
		Total          int64   `gorm:"column:total"`
		SuccessCount   int64   `gorm:"column:success_count"`
		ErrorCount     int64   `gorm:"column:error_count"`
		AverageLatency float64 `gorm:"column:average_latency"`
		TotalCost      float64 `gorm:"column:total_cost"`
		TotalUnits     float64 `gorm:"column:total_units"`
	}
	if err := query.Select(`
			COUNT(*) as total,
			SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END) as success_count,
			SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END) as error_count,
			COALESCE(AVG(latency), 0) as average_latency,
			COALESCE(SUM(cost), 0) as total_cost,
			COALESCE(SUM(billed_units), 0) as total_units
		`).Scan(&row).Error; err != nil {
		return nil, fmt.Errorf("failed to get relay log stats: %w", err)
	}
	stats := &RelayLogStats{
		TotalRequests: row.Total, SuccessCount: row.SuccessCount, ErrorCount: row.ErrorCount,
		AverageLatency: row.AverageLatency, TotalCost: row.TotalCost, TotalUnits: row.TotalUnits,
	}
	if terminal := row.SuccessCount + row.ErrorCount; terminal > 0 {
		stats.SuccessRate = float64(row.SuccessCount) / float64(terminal) * 100
	}
	return stats, nil
}

// GetRelayHistogram returns time-bucketed volume and spend for the filters.
func (s *RDBLogStore) GetRelayHistogram(ctx context.Context, filters RelayLogFilters, bucketSizeSeconds int64) (*RelayHistogramResult, error) {
	if bucketSizeSeconds <= 0 {
		bucketSizeSeconds = 3600
	}
	query := s.applyRelayLogFilters(s.ScopedDB(ctx).Model(&RelayLog{}), filters)
	var results []struct {
		BucketTimestamp int64   `gorm:"column:bucket_timestamp"`
		Count           int64   `gorm:"column:count"`
		Success         int64   `gorm:"column:success"`
		Error           int64   `gorm:"column:error"`
		Cancelled       int64   `gorm:"column:cancelled_count"`
		Cost            float64 `gorm:"column:total_cost"`
	}
	selectClause := fmt.Sprintf(`
			%s as bucket_timestamp,
			COUNT(*) as count,
			SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END) as success,
			SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END) as error,
			SUM(CASE WHEN status = 'cancelled' THEN 1 ELSE 0 END) as cancelled_count,
			COALESCE(SUM(cost), 0) as total_cost
		`, unixBucketExpr(s.db.Dialector.Name(), bucketSizeSeconds))
	if err := query.Select(selectClause).Group("bucket_timestamp").Order("bucket_timestamp ASC").Find(&results).Error; err != nil {
		return nil, fmt.Errorf("failed to get relay histogram: %w", err)
	}
	byTimestamp := make(map[int64]RelayHistogramBucket, len(results))
	for _, r := range results {
		byTimestamp[r.BucketTimestamp] = RelayHistogramBucket{Timestamp: time.Unix(r.BucketTimestamp, 0).UTC(), Count: r.Count, Success: r.Success, Error: r.Error, Cancelled: r.Cancelled, Cost: r.Cost}
	}
	all := generateBucketTimestamps(filters.StartTime, filters.EndTime, bucketSizeSeconds)
	if len(all) == 0 {
		buckets := make([]RelayHistogramBucket, 0, len(results))
		for _, r := range results {
			buckets = append(buckets, byTimestamp[r.BucketTimestamp])
		}
		return &RelayHistogramResult{Buckets: buckets, BucketSizeSeconds: bucketSizeSeconds}, nil
	}
	buckets := make([]RelayHistogramBucket, len(all))
	for i, ts := range all {
		if bucket, ok := byTimestamp[ts]; ok {
			buckets[i] = bucket
			continue
		}
		buckets[i] = RelayHistogramBucket{Timestamp: time.Unix(ts, 0).UTC()}
	}
	return &RelayHistogramResult{Buckets: buckets, BucketSizeSeconds: bucketSizeSeconds}, nil
}

// GetRelayTopEndpoints ranks endpoints by request count for the filters.
func (s *RDBLogStore) GetRelayTopEndpoints(ctx context.Context, filters RelayLogFilters, limit int) (*RelayTopResults, error) {
	if limit <= 0 {
		limit = 10
	}
	query := s.applyRelayLogFilters(s.ScopedDB(ctx).Model(&RelayLog{}), filters)
	// total_cost, not cost: ClickHouse resolves a SELECT alias in WHERE, and the cost filters would
	// then name an aggregate.
	var rows []struct {
		RelayName      string  `gorm:"column:relay_name"`
		Count          int64   `gorm:"column:count"`
		ErrorCount     int64   `gorm:"column:error_count"`
		TotalCost      float64 `gorm:"column:total_cost"`
		AverageLatency float64 `gorm:"column:average_latency"`
	}
	if err := query.
		Select("relay_name, COUNT(*) as count, SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END) as error_count, COALESCE(SUM(cost), 0) as total_cost, COALESCE(AVG(latency), 0) as average_latency").
		Group("relay_name").
		Order("count DESC, relay_name ASC").
		Limit(limit).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("failed to get top relays: %w", err)
	}
	results := make([]RelayTopResult, 0, len(rows))
	for _, r := range rows {
		results = append(results, RelayTopResult{RelayName: r.RelayName, Count: r.Count, ErrorCount: r.ErrorCount, Cost: r.TotalCost, AverageLatency: r.AverageLatency})
	}
	return &RelayTopResults{Endpoints: results}, nil
}

// GetRelayFilterData lists distinct endpoint names, methods and virtual keys seen in
// the logs, for the filter sidebar.
func (s *RDBLogStore) GetRelayFilterData(ctx context.Context, limit int) (*RelayFilterData, error) {
	if limit <= 0 {
		limit = 100
	}
	out := &RelayFilterData{EndpointNames: []string{}, Methods: []string{}, VirtualKeys: []RelayFilterOption{}}
	if err := s.ScopedDB(ctx).Model(&RelayLog{}).Distinct().Order("relay_name").Limit(limit).Pluck("relay_name", &out.EndpointNames).Error; err != nil {
		return nil, err
	}
	if err := s.ScopedDB(ctx).Model(&RelayLog{}).Distinct().Where("method <> ''").Order("method").Limit(limit).Pluck("method", &out.Methods).Error; err != nil {
		return nil, err
	}
	var keys []struct {
		ID   string `gorm:"column:virtual_key_id"`
		Name string `gorm:"column:virtual_key_name"`
	}
	if err := s.ScopedDB(ctx).Model(&RelayLog{}).
		Select("virtual_key_id, MAX(virtual_key_name) as virtual_key_name").
		Where("virtual_key_id IS NOT NULL AND virtual_key_id <> ''").
		Group("virtual_key_id").Order("virtual_key_id").Limit(limit).Scan(&keys).Error; err != nil {
		return nil, err
	}
	for _, k := range keys {
		out.VirtualKeys = append(out.VirtualKeys, RelayFilterOption{ID: k.ID, Name: k.Name})
	}
	return out, nil
}

// DeleteRelayLogs deletes the identified rows, with the job rows that settle them.
func (s *RDBLogStore) DeleteRelayLogs(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.ScopedDB(ctx).Where("id IN ? OR parent_log_id IN ?", ids, ids).Delete(&RelayLog{}).Error
}

// DeleteRelayLogsBatch deletes up to batchSize rows older than cutoff, oldest first,
// for the retention cleaner.
func (s *RDBLogStore) DeleteRelayLogsBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	if s.db.Dialector.Name() == "clickhouse" {
		return 0, nil // ClickHouseLogStore overrides this with a lightweight delete
	}
	return s.deleteExpiredBatch(ctx, "relay_logs", "timestamp", cutoff, batchSize)
}

// HasRelayLogs reports whether any row is stored, for the empty state.
func (s *RDBLogStore) HasRelayLogs(ctx context.Context) (bool, error) {
	var ids []string
	if err := s.ScopedDB(ctx).Model(&RelayLog{}).Limit(1).Pluck("id", &ids).Error; err != nil {
		return false, err
	}
	return len(ids) > 0, nil
}
