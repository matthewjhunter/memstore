package pgstore

import (
	"context"
	"errors"
	"testing"

	"github.com/matthewjhunter/go-embedding"

	"github.com/matthewjhunter/memstore"
)

// The first vector written records the store's dimension in memstore_meta,
// whichever path writes it, and a vector of another dimension is refused rather
// than stored: vectors of two widths in one store cannot be compared. Only the
// EmbedFacts backfill used to record it, so a store embedded by the queue never
// had a dimension on record and the check at open never fired (#236).

func storedDim(t *testing.T, s *PostgresStore) string {
	t.Helper()
	var v string
	err := s.pool.QueryRow(context.Background(),
		`SELECT coalesce((SELECT value FROM memstore_meta WHERE key = 'embedding_dim'), '')`).Scan(&v)
	if err != nil {
		t.Fatalf("reading embedding_dim: %v", err)
	}
	return v
}

// vectorsOfDim counts the stored fact and chunk vectors of dimension dim.
func vectorsOfDim(t *testing.T, s *PostgresStore, dim int) int {
	t.Helper()
	var n int
	err := s.pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM memstore_facts WHERE vector_dims(embedding) = $1)
		      + (SELECT count(*) FROM memstore_fact_chunks WHERE vector_dims(embedding) = $1)`, dim).Scan(&n)
	if err != nil {
		t.Fatalf("counting vectors: %v", err)
	}
	return n
}

func unitVec(dim int) []float32 {
	v := make([]float32, dim)
	v[0] = 1
	return v
}

func insertPlainFact(t *testing.T, s *PostgresStore) int64 {
	t.Helper()
	id, err := s.Insert(context.Background(), memstore.Fact{Content: "a fact about vectors", Subject: "dim", Category: "note"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	return id
}

func writeFactVectors(t *testing.T, s *PostgresStore, dim int) error {
	v := unitVec(dim)
	return s.SetFactVectors(context.Background(), insertPlainFact(t, s),
		memstore.FactVectors{Whole: v, Chunks: []memstore.FactChunk{{Vector: v, ByteEnd: 4}}})
}

func TestVectorWritesRecordDimension(t *testing.T) {
	ctx := context.Background()
	paths := []struct {
		name  string
		write func(t *testing.T, s *PostgresStore, dim int) error
	}{
		{"SetFactVectors", writeFactVectors},
		{"SetEmbedding", func(t *testing.T, s *PostgresStore, dim int) error {
			return s.SetEmbedding(ctx, insertPlainFact(t, s), unitVec(dim))
		}},
		{"Insert", func(t *testing.T, s *PostgresStore, dim int) error {
			_, err := s.Insert(ctx, memstore.Fact{Content: "embedded at insert", Subject: "dim", Category: "note", Embedding: unitVec(dim)})
			return err
		}},
		{"InsertBatch", func(t *testing.T, s *PostgresStore, dim int) error {
			return s.InsertBatch(ctx, []memstore.Fact{{Content: "embedded in a batch", Subject: "dim", Category: "note", Embedding: unitVec(dim)}})
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			s := newANNStore(t)
			if got := storedDim(t, s); got != "" {
				t.Fatalf("fresh store has embedding_dim %q, want none", got)
			}
			if err := p.write(t, s, 4); err != nil {
				t.Fatalf("first write: %v", err)
			}
			if got := storedDim(t, s); got != "4" {
				t.Errorf("embedding_dim after the first write = %q, want 4", got)
			}

			err := p.write(t, s, 8)
			if !errors.Is(err, embedding.ErrDimChanged) {
				t.Errorf("writing an 8-dimensional vector: err = %v, want ErrDimChanged", err)
			}
			if got := storedDim(t, s); got != "4" {
				t.Errorf("embedding_dim after the refused write = %q, want 4", got)
			}
			if n := vectorsOfDim(t, s, 8); n != 0 {
				t.Errorf("%d 8-dimensional vectors stored, want the write refused whole", n)
			}
		})
	}
}

// Clearing vectors forgets the dimension, in the database and in the store's
// cache of it: the vectors that replace them may come from another model. A
// cache left behind would skip the next write's check and leave the new
// dimension unrecorded, which is this bug again.
func TestClearVectorsForgetsDimension(t *testing.T) {
	s := newANNStore(t)
	if err := writeFactVectors(t, s, 4); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := s.clearVectors(context.Background()); err != nil {
		t.Fatalf("clearVectors: %v", err)
	}
	if got := storedDim(t, s); got != "" {
		t.Errorf("embedding_dim after clearVectors = %q, want none", got)
	}
	if err := writeFactVectors(t, s, 8); err != nil {
		t.Fatalf("write after clearVectors: %v", err)
	}
	if got := storedDim(t, s); got != "8" {
		t.Errorf("embedding_dim after rewriting = %q, want 8", got)
	}
}

// A store that has vectors but no dimension on record -- the live one -- gets
// it recorded at open, from the vectors, so the check works from then on.
func TestOpenRecordsMissingDimension(t *testing.T) {
	ctx := context.Background()
	s := newANNStore(t)
	if err := writeFactVectors(t, s, 4); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM memstore_meta WHERE key = 'embedding_dim'`); err != nil {
		t.Fatalf("clearing embedding_dim: %v", err)
	}

	reopened, err := New(ctx, s.pool, nil, "test", 0, 0)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if got := storedDim(t, reopened); got != "4" {
		t.Errorf("embedding_dim after reopening = %q, want 4 from the stored vectors", got)
	}
	if err := writeFactVectors(t, reopened, 8); !errors.Is(err, embedding.ErrDimChanged) {
		t.Errorf("8-dimensional write after reopening: err = %v, want ErrDimChanged", err)
	}
}
