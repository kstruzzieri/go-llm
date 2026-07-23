// Package parquet exports vector store data to Apache Parquet format
// for consumption by Python ML pipelines.
//
// The package is intentionally isolated from the core rag package —
// consumers that don't import rag/parquet never pull in the Parquet dependency.
//
// Use [ExportDataset] with any [rag.Exportable] implementation, or
// [ExportVectorStore] as a convenience wrapper for [rag.VectorStore] values.
package parquet
