package http

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBAdminHandler provides generic database browsing endpoints
type DBAdminHandler struct {
	pool   *pgxpool.Pool
	schema string
}

func NewDBAdminHandler(pool *pgxpool.Pool, schema string) *DBAdminHandler {
	return &DBAdminHandler{pool: pool, schema: schema}
}

// sensitiveColumns that should be masked in output
var sensitivePatterns = []string{"password", "hash", "secret", "api_key", "token", "private_key"}

func isSensitiveColumn(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range sensitivePatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// ListTables returns all tables with approximate row counts
// GET /tables
func (h *DBAdminHandler) ListTables(c *gin.Context) {
	rows, err := h.pool.Query(c.Request.Context(), `
		SELECT t.table_name, COALESCE(s.n_live_tup, 0)::int AS row_count
		FROM information_schema.tables t
		LEFT JOIN pg_stat_user_tables s
		  ON s.schemaname = t.table_schema AND s.relname = t.table_name
		WHERE t.table_schema = $1 AND t.table_type = 'BASE TABLE'
		ORDER BY t.table_name
	`, h.schema)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	type tableInfo struct {
		Name     string `json:"name"`
		RowCount int    `json:"row_count"`
	}
	var tables []tableInfo
	for rows.Next() {
		var t tableInfo
		if err := rows.Scan(&t.Name, &t.RowCount); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		tables = append(tables, t)
	}
	if tables == nil {
		tables = []tableInfo{}
	}

	c.JSON(http.StatusOK, gin.H{"tables": tables})
}

// GetTableSchema returns column definitions for a table
// GET /tables/:table/schema
func (h *DBAdminHandler) GetTableSchema(c *gin.Context) {
	table := c.Param("table")

	// Validate table exists
	if !h.tableExists(c.Request.Context(), table) {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("table %q not found", table)})
		return
	}

	// Get columns
	rows, err := h.pool.Query(c.Request.Context(), `
		SELECT column_name, data_type, is_nullable, column_default,
		       character_maximum_length, ordinal_position
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position
	`, h.schema, table)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	// Get primary key columns
	pkCols := h.getPrimaryKeys(c.Request.Context(), table)

	type columnInfo struct {
		Name      string  `json:"name"`
		Type      string  `json:"type"`
		Nullable  bool    `json:"nullable"`
		Default   *string `json:"default,omitempty"`
		MaxLength *int    `json:"max_length,omitempty"`
		IsPrimary bool    `json:"is_primary"`
	}

	var columns []columnInfo
	for rows.Next() {
		var col columnInfo
		var isNullable string
		var ordinal int
		if err := rows.Scan(&col.Name, &col.Type, &isNullable, &col.Default, &col.MaxLength, &ordinal); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		col.Nullable = isNullable == "YES"
		col.IsPrimary = pkCols[col.Name]
		columns = append(columns, col)
	}
	if columns == nil {
		columns = []columnInfo{}
	}

	c.JSON(http.StatusOK, gin.H{"table": table, "columns": columns})
}

// QueryRows returns paginated rows with optional search and sort
// GET /tables/:table/rows?page=1&page_size=50&search=&sort_by=created_at&sort_order=desc
func (h *DBAdminHandler) QueryRows(c *gin.Context) {
	table := c.Param("table")

	// Validate table
	if !h.tableExists(c.Request.Context(), table) {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("table %q not found", table)})
		return
	}

	// Parse params
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	search := c.Query("search")
	sortBy := c.DefaultQuery("sort_by", "")
	sortOrder := c.DefaultQuery("sort_order", "desc")

	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 50
	}
	if sortOrder != "asc" && sortOrder != "desc" {
		sortOrder = "desc"
	}

	ctx := c.Request.Context()

	// Get column info for building queries
	colInfo := h.getColumnInfo(ctx, table)
	if len(colInfo) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read column info"})
		return
	}

	// Validate sort_by
	if sortBy != "" {
		valid := false
		for _, ci := range colInfo {
			if ci.name == sortBy {
				valid = true
				break
			}
		}
		if !valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid sort_by column: %q", sortBy)})
			return
		}
	}

	// Build query
	qualifiedTable := fmt.Sprintf("%q.%q", h.schema, table)
	var whereClauses []string
	var args []interface{}
	argIdx := 1

	if search != "" {
		var searchConds []string
		for _, ci := range colInfo {
			if ci.isTextType {
				searchConds = append(searchConds, fmt.Sprintf("%q::text ILIKE '%%' || $%d || '%%'", ci.name, argIdx))
			}
		}
		if len(searchConds) > 0 {
			whereClauses = append(whereClauses, "("+strings.Join(searchConds, " OR ")+")")
			args = append(args, search)
			argIdx++
		}
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Count total
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM %s %s", qualifiedTable, whereSQL)
	var total int
	if err := h.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Build ORDER BY
	orderSQL := ""
	if sortBy != "" {
		orderSQL = fmt.Sprintf("ORDER BY %q %s", sortBy, sortOrder)
	}

	// Fetch rows
	offset := (page - 1) * pageSize
	dataQuery := fmt.Sprintf("SELECT * FROM %s %s %s LIMIT $%d OFFSET $%d",
		qualifiedTable, whereSQL, orderSQL, argIdx, argIdx+1)
	args = append(args, pageSize, offset)

	dataRows, err := h.pool.Query(ctx, dataQuery, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer dataRows.Close()

	// Scan rows generically
	fields := dataRows.FieldDescriptions()
	var results []map[string]interface{}
	for dataRows.Next() {
		values, err := dataRows.Values()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		row := make(map[string]interface{}, len(fields))
		for i, fd := range fields {
			name := string(fd.Name)
			if isSensitiveColumn(name) {
				row[name] = "***"
			} else {
				row[name] = formatValue(values[i])
			}
		}
		results = append(results, row)
	}
	if results == nil {
		results = []map[string]interface{}{}
	}

	c.JSON(http.StatusOK, gin.H{
		"table":     table,
		"rows":      results,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// formatValue converts pgx native types to JSON-friendly representations.
// In particular, [16]byte (UUID) is formatted as a standard UUID string.
func formatValue(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case [16]byte:
		// UUID: format as xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
		h := hex.EncodeToString(val[:])
		return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	default:
		return v
	}
}

// --- helpers ---

func (h *DBAdminHandler) tableExists(ctx context.Context, table string) bool {
	var exists bool
	h.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1 AND table_name = $2 AND table_type = 'BASE TABLE'
		)
	`, h.schema, table).Scan(&exists)
	return exists
}

func (h *DBAdminHandler) getPrimaryKeys(ctx context.Context, table string) map[string]bool {
	pks := make(map[string]bool)
	rows, err := h.pool.Query(ctx, `
		SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema
		WHERE tc.table_schema = $1 AND tc.table_name = $2 AND tc.constraint_type = 'PRIMARY KEY'
	`, h.schema, table)
	if err != nil {
		return pks
	}
	defer rows.Close()
	for rows.Next() {
		var col string
		rows.Scan(&col)
		pks[col] = true
	}
	return pks
}

type colMeta struct {
	name       string
	isTextType bool
}

func (h *DBAdminHandler) getColumnInfo(ctx context.Context, table string) []colMeta {
	rows, err := h.pool.Query(ctx, `
		SELECT column_name, data_type
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position
	`, h.schema, table)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var cols []colMeta
	for rows.Next() {
		var name, dtype string
		rows.Scan(&name, &dtype)
		isText := strings.Contains(dtype, "char") || strings.Contains(dtype, "text") || dtype == "uuid"
		cols = append(cols, colMeta{name: name, isTextType: isText})
	}
	return cols
}
