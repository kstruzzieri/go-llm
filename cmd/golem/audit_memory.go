package main

import (
	"context"
	"database/sql"
	"errors"
	"os"

	"github.com/kstruzzieri/go-llm/memory"
)

func auditMemory(ctx context.Context, root string) (result auditResult) {
	defer func() { result.scope, result.assurance = "memory", "signed" }()
	path, err := memoryDBPathForWorkspace(os.Getenv, root)
	if err != nil {
		return auditResult{outcome: "incomplete", diagnostics: []auditDiagnostic{{code: "store-unavailable", message: "Store is unavailable or unsafe."}}}
	}
	return withAuditStore(ctx, path, func(db *sql.DB) auditResult {
		report, err := memory.AuditRecords(ctx, db, path+".keys")
		return auditMemoryResult(report, err, path)
	})
}

func auditMemoryResult(report memory.RecordAuditReport, err error, path string) auditResult {
	result := auditResult{checked: report.Verified, total: report.TotalExtant}
	if err == nil {
		if report.Configured {
			result.outcome = "valid"
		} else {
			result.outcome = "not-configured"
		}
		return result
	}
	if report.TotalExtant != nil && report.Verified < *report.TotalExtant {
		result.earlyStop = true
	}
	if errors.Is(err, memory.ErrRecordAuditViolation) {
		result.outcome = "violation"
		result.diagnostics = []auditDiagnostic{{code: "memory-record-invalid", target: path, message: "Stored agent-memory record integrity verification failed."}}
		return result
	}
	result.outcome = "incomplete"
	result.diagnostics = []auditDiagnostic{{code: "memory-record-incomplete", target: path, message: "Agent-memory record verification could not be completed."}}
	return result
}
