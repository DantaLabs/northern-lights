// Package workivaprovider defines the internal boundary between Northern
// Lights tools and Workiva transports. The REST client remains the primary
// backend. Alternate transports, including Workiva's official MCP server, can
// replace read or write capabilities independently without changing tool
// schemas, mapping, confirmation, or audit behavior.
package workivaprovider

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// Reader is the Workiva capability required by discovery, range reads, field
// reads, and mapping synchronization.
type Reader interface {
	ListSpreadsheets(ctx context.Context) ([]workiva.Spreadsheet, error)
	ListSheets(ctx context.Context, spreadsheetID string) ([]workiva.Sheet, error)
	GetSheetData(ctx context.Context, spreadsheetID, sheetID, cellRange string, fields []string) (*workiva.SheetData, error)
}

// Writer is the Workiva capability required by the controlled-write flow.
// Keeping it separate from Reader permits official MCP reads while the proven
// REST implementation remains responsible for writes.
type Writer interface {
	UpdateSheetWithRetryAfter(ctx context.Context, spreadsheetID, sheetID string, update workiva.SheetUpdate) (operationURL string, initialRetryAfter time.Duration, err error)
	WaitOperationWithInitialRetryAfter(ctx context.Context, operationURL string, initialRetryAfter time.Duration) (resourceURL string, err error)
}

// OperationInspection is read-only provider evidence for a previously
// submitted asynchronous operation. It is intentionally separate from Writer
// so reconciliation cannot gain a mutation capability by accident.
type OperationInspection = workiva.OperationInspection

type OperationReader interface {
	InspectOperation(context.Context, string) (OperationInspection, error)
}

// Backend is a complete Workiva transport. The existing *workiva.Client
// satisfies this interface and remains the default implementation.
type Backend interface {
	Reader
	Writer
}

// Router presents one stable backend to Northern Lights tools while allowing
// capabilities to move to another implementation independently.
type Router struct {
	primary Backend
	reader  Reader
	writer  Writer
}

// Option configures a capability override. Overrides are explicit: the
// primary backend continues handling every capability not replaced.
type Option func(*Router)

// WithReadBackend routes discovery and sheet reads to reader. Writes and
// operation polling remain on the primary backend.
func WithReadBackend(reader Reader) Option {
	return func(router *Router) {
		if reader != nil && !isNilInterface(reader) {
			router.reader = reader
		}
	}
}

// WithWriteBackend routes mutations and operation polling to writer. It is
// intentionally separate from WithReadBackend because Workiva's official MCP
// may expose different capabilities over time.
func WithWriteBackend(writer Writer) Option {
	return func(router *Router) {
		if writer != nil && !isNilInterface(writer) {
			router.writer = writer
		}
	}
}

// NewRouter builds a router around a required primary backend. It panics on a
// nil primary because this constructor is intended for application wiring,
// where the absence of Workiva transport is a programming error.
func NewRouter(primary Backend, options ...Option) *Router {
	router, err := NewRouterChecked(primary, options...)
	if err != nil {
		panic(err)
	}
	return router
}

// NewRouterChecked is the error-returning constructor for dynamic wiring and
// tests.
func NewRouterChecked(primary Backend, options ...Option) (*Router, error) {
	if primary == nil || isNilInterface(primary) {
		return nil, errors.New("workivaprovider: primary backend is required")
	}
	router := &Router{primary: primary, reader: primary, writer: primary}
	for _, option := range options {
		if option != nil {
			option(router)
		}
	}
	return router, nil
}

func isNilInterface(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (r *Router) ListSpreadsheets(ctx context.Context) ([]workiva.Spreadsheet, error) {
	return r.reader.ListSpreadsheets(ctx)
}

func (r *Router) ListSheets(ctx context.Context, spreadsheetID string) ([]workiva.Sheet, error) {
	return r.reader.ListSheets(ctx, spreadsheetID)
}

func (r *Router) GetSheetData(ctx context.Context, spreadsheetID, sheetID, cellRange string, fields []string) (*workiva.SheetData, error) {
	return r.reader.GetSheetData(ctx, spreadsheetID, sheetID, cellRange, fields)
}

// ReadUncached performs a distinct direct provider read for assurance. The
// existing range cache is intentionally not consulted or populated.
func (r *Router) ReadUncached(ctx context.Context, request assurance.SourceRequest) (assurance.ProviderRead, error) {
	if typed, ok := r.reader.(assurance.SourceReader); ok {
		return typed.ReadUncached(ctx, request)
	}
	if request.Consistency == assurance.ConsistencyRevisionPinned {
		return assurance.ProviderRead{}, fmt.Errorf("revision-pinned reads are unsupported")
	}
	fields := []string{"cells.value", "cells.calculatedValue"}
	var data *workiva.SheetData
	var err error
	if typed, ok := r.reader.(interface {
		GetSheetDataTyped(context.Context, string, string, string, []string) (*workiva.SheetData, error)
	}); ok {
		data, err = typed.GetSheetDataTyped(ctx, request.ExternalResourceID, request.SubresourceID, request.Locator, fields)
	} else {
		data, err = r.reader.GetSheetData(ctx, request.ExternalResourceID, request.SubresourceID, request.Locator, fields)
	}
	if err != nil {
		return assurance.ProviderRead{}, err
	}
	if data == nil || len(data.Cells) != 1 || len(data.Cells[0]) != 1 {
		return assurance.ProviderRead{}, fmt.Errorf("uncached assurance source must resolve to exactly one cell")
	}
	cell := data.Cells[0][0]
	value := assurance.ProviderValue{Value: cell.Value}
	if formula, ok := cell.Value.(string); ok && strings.HasPrefix(formula, "=") {
		value.Formula = formula
		value.CalculatedValue = cell.CalculatedValue
	}
	return assurance.ProviderRead{
		Value:            value,
		ProviderRevision: assurance.ProviderRevision{Strength: assurance.RevisionUnavailable},
		CacheBypassed:    true,
	}, nil
}

// SupportsRevisionPinning is false until a provider returns and enforces a
// real revision/ETag contract. It must never be inferred from timestamps.
func (r *Router) SupportsRevisionPinning() bool {
	if capability, ok := r.reader.(assurance.RevisionPinCapability); ok {
		return capability.SupportsRevisionPinning()
	}
	return false
}

func (r *Router) UpdateSheetWithRetryAfter(ctx context.Context, spreadsheetID, sheetID string, update workiva.SheetUpdate) (string, time.Duration, error) {
	return r.writer.UpdateSheetWithRetryAfter(ctx, spreadsheetID, sheetID, update)
}

func (r *Router) WaitOperationWithInitialRetryAfter(ctx context.Context, operationURL string, initialRetryAfter time.Duration) (string, error) {
	return r.writer.WaitOperationWithInitialRetryAfter(ctx, operationURL, initialRetryAfter)
}

// InspectOperation only delegates to an explicitly implemented read-only
// operation reader. There is no fallback through submit or wait, because a
// reconciliation inspection must preserve provider-call semantics.
func (r *Router) InspectOperation(ctx context.Context, operationURL string) (OperationInspection, error) {
	reader, ok := r.writer.(OperationReader)
	if !ok {
		return OperationInspection{}, errors.New("workivaprovider: read-only operation inspection is unsupported")
	}
	return reader.InspectOperation(ctx, operationURL)
}

var _ Backend = (*workiva.Client)(nil)
var _ Backend = (*Router)(nil)
