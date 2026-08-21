package store

import (
	"database/sql"
	"encoding/binary"
	"math"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct{ db *sql.DB }

type Person struct {
	ID         string `json:"id"`
	CreatedAt  string `json:"created_at"`
	Embeddings int    `json:"embeddings"`
}

type Embedding struct {
	PersonID  string
	Embedding []float32
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS persons (
	id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS embeddings (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	person_id TEXT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
	embedding BLOB NOT NULL,
	dimensions INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	preview BLOB
);
CREATE INDEX IF NOT EXISTS idx_embeddings_person ON embeddings(person_id);
`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) AddEmbedding(id string, v []float32, preview []byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO persons(id, created_at) VALUES (?, ?)`,
		id, now,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(
		`INSERT INTO embeddings(person_id, embedding, dimensions, created_at, preview)
		 VALUES (?, ?, ?, ?, ?)`,
		id, encodeFloats(v), len(v), now, preview,
	); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) Get(id string) (Person, bool, error) {
	var p Person
	err := s.db.QueryRow(`
SELECT p.id, p.created_at, COUNT(e.id)
FROM persons p
LEFT JOIN embeddings e ON e.person_id = p.id
WHERE p.id = ?
GROUP BY p.id`, id).Scan(&p.ID, &p.CreatedAt, &p.Embeddings)

	if err == sql.ErrNoRows {
		return Person{}, false, nil
	}
	if err != nil {
		return Person{}, false, err
	}
	return p, true, nil
}

func (s *Store) List() ([]Person, error) {
	rows, err := s.db.Query(`
SELECT p.id, p.created_at, COUNT(e.id)
FROM persons p
LEFT JOIN embeddings e ON e.person_id = p.id
GROUP BY p.id
ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Person{}
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.CreatedAt, &p.Embeddings); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Delete(id string) (bool, error) {
	if _, err := s.db.Exec(`DELETE FROM embeddings WHERE person_id = ?`, id); err != nil {
		return false, err
	}
	res, err := s.db.Exec(`DELETE FROM persons WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) Counts() (int, int, error) {
	var p, e int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&p); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM embeddings`).Scan(&e); err != nil {
		return 0, 0, err
	}
	return p, e, nil
}

func (s *Store) Embeddings() ([]Embedding, error) {
	rows, err := s.db.Query(`SELECT person_id, embedding, dimensions FROM embeddings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Embedding
	for rows.Next() {
		var id string
		var blob []byte
		var n int
		if err := rows.Scan(&id, &blob, &n); err != nil {
			return nil, err
		}
		out = append(out, Embedding{id, decodeFloats(blob, n)})
	}
	return out, rows.Err()
}

func encodeFloats(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

func decodeFloats(b []byte, n int) []float32 {
	if n < 0 || len(b) < n*4 {
		return nil
	}
	v := make([]float32, n)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
