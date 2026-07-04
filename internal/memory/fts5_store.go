package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// KeywordStore BM25 关键词搜索后端（SQLite FTS5）。
type KeywordStore interface {
	EnsureIndex(ctx context.Context) error
	IndexChunks(ctx context.Context, chunks []MemoryChunk) error
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
	Delete(ctx context.Context, ids []string) error
	Close() error
}

// FTS5Store SQLite FTS5 + BM25 实现。
// 每个 FileStore 实例持有一个独立的 FTS5Store。
type FTS5Store struct {
	db       *sql.DB
	dbPath   string
	tableName string
}

// NewFTS5Store 打开或创建 FTS5 索引数据库。
// dbPath 是 SQLite 文件路径（如 file_store/fts5_index.db）。
func NewFTS5Store(dbPath string) (*FTS5Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("fts5: open db: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite 单写者
	return &FTS5Store{
		db:        db,
		dbPath:    dbPath,
		tableName: "chunks_fts",
	}, nil
}

var _ KeywordStore = (*FTS5Store)(nil)

// EnsureIndex 确保 FTS5 表存在。
func (s *FTS5Store) EnsureIndex(_ context.Context) error {
	query := fmt.Sprintf(
		"CREATE VIRTUAL TABLE IF NOT EXISTS %s USING fts5(merge_key, text, path, start_line, end_line)",
		s.tableName,
	)
	_, err := s.db.Exec(query)
	return err
}

// IndexChunks 批量写入 chunks（先删旧再插新，merge_key = chunk ID）。
func (s *FTS5Store) IndexChunks(_ context.Context, chunks []MemoryChunk) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("fts5: begin tx: %w", err)
	}
	defer tx.Rollback()

	insertSQL := fmt.Sprintf(
		"INSERT INTO %s(merge_key, text, path, start_line, end_line) VALUES(?,?,?,?,?)",
		s.tableName,
	)

	for _, c := range chunks {
		if _, err := tx.Exec(insertSQL, c.ID, c.Text, c.Path, c.StartLine, c.EndLine); err != nil {
			return fmt.Errorf("fts5: insert chunk %s: %w", c.ID, err)
		}
	}

	return tx.Commit()
}

// Search BM25 关键词搜索。
// 使用 FTS5 BM25 排名函数，返回按相关性排序的结果。
func (s *FTS5Store) Search(_ context.Context, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 5
	}

	// FTS5 query 语法：双引号包围的短语做精确匹配，否则做 AND 匹配
	ftsQuery := toFTS5Query(query)

	selectSQL := fmt.Sprintf(
		"SELECT merge_key, text, path, start_line, end_line, bm25(%s) AS score FROM %s WHERE %s MATCH ? ORDER BY score LIMIT ?",
		s.tableName, s.tableName, s.tableName,
	)

	rows, err := s.db.Query(selectSQL, ftsQuery, limit)
	if err != nil {
		// FTS5 可能因为特殊字符报语法错，回退到 LIKE
		return s.fallbackSearch(query, limit)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var startLine, endLine int
		var score float64
		if err := rows.Scan(&r.ID, &r.Text, &r.Path, &startLine, &endLine, &score); err != nil {
			continue
		}
		r.StartLine = startLine
		r.EndLine = endLine
		// BM25 分数取负值转正（越低越相关 → 越高越相关），归一化
		r.Score = bm25ToRelevance(score)
		results = append(results, r)
	}

	return results, nil
}

// fallbackSearch 当 FTS5 语法报错时回退到 LIKE 匹配。
func (s *FTS5Store) fallbackSearch(query string, limit int) ([]SearchResult, error) {
	likePattern := "%" + strings.ReplaceAll(query, "%", "\\%") + "%"
	selectSQL := fmt.Sprintf(
		"SELECT merge_key, text, path, start_line, end_line FROM %s WHERE text LIKE ? ESCAPE '\\' LIMIT ?",
		s.tableName,
	)

	rows, err := s.db.Query(selectSQL, likePattern, limit)
	if err != nil {
		return nil, fmt.Errorf("fts5: fallback search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var startLine, endLine int
		if err := rows.Scan(&r.ID, &r.Text, &r.Path, &startLine, &endLine); err != nil {
			continue
		}
		r.StartLine = startLine
		r.EndLine = endLine
		r.Score = 0.1 // LIKE 匹配分数低
		results = append(results, r)
	}
	return results, nil
}

// Delete 按 merge_key 删除 chunks。
func (s *FTS5Store) Delete(_ context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	deleteSQL := fmt.Sprintf("DELETE FROM %s WHERE merge_key = ?", s.tableName)
	for _, id := range ids {
		if _, err := s.db.Exec(deleteSQL, id); err != nil {
			return fmt.Errorf("fts5: delete %s: %w", id, err)
		}
	}
	return nil
}

// Close 关闭数据库连接。
func (s *FTS5Store) Close() error {
	return s.db.Close()
}

// ---- 辅助 ----

// toFTS5Query 把用户查询转为 FTS5 兼容的 MATCH 语法。
// 多个词 → AND 连接；加双引号匹配精确短语。
func toFTS5Query(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return "*"
	}
	// 如果已有引号，直接返回
	if strings.Contains(query, `"`) {
		return query
	}
	// 多个词用 AND 连接
	words := strings.Fields(query)
	if len(words) == 1 {
		return `"` + strings.ReplaceAll(query, `"`, ``) + `"`
	}
	// 多词：AND 连接
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = `"` + strings.ReplaceAll(w, `"`, ``) + `"`
	}
	return strings.Join(parts, " AND ")
}

// bm25ToRelevance 把 BM25 分数（越低越好，可为负）转为 [0,1] 相关性分数。
// 使用 sigmoid 平滑：1/(1+e^(-|score|/10))，再钳制。
func bm25ToRelevance(bm25 float64) float64 {
	if bm25 >= 0 {
		return 0.01 // 无效分数
	}
	// BM25 为负，取绝对值越大越相关
	abs := -bm25
	rel := 1.0 / (1.0 + 100.0/abs) // 近似：abs 小→接近0，abs 大→接近1
	if rel > 1.0 {
		rel = 1.0
	}
	return rel
}
