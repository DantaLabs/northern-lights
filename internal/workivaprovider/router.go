// Package workivaprovider defines the internal boundary between Northern
// Lights tools and Workiva transports. The REST client remains the primary
// backend. Alternate transports, including Workiva's official MCP server, can
// replace read or write capabilities independently without changing tool
// schemas, mapping, confirmation, or audit behavior.
package workivaprovider

import (
	"context"
	"errors"
	"reflect"
	"time"

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

func (r *Router) UpdateSheetWithRetryAfter(ctx context.Context, spreadsheetID, sheetID string, update workiva.SheetUpdate) (string, time.Duration, error) {
	return r.writer.UpdateSheetWithRetryAfter(ctx, spreadsheetID, sheetID, update)
}

func (r *Router) WaitOperationWithInitialRetryAfter(ctx context.Context, operationURL string, initialRetryAfter time.Duration) (string, error) {
	return r.writer.WaitOperationWithInitialRetryAfter(ctx, operationURL, initialRetryAfter)
}

var _ Backend = (*workiva.Client)(nil)
var _ Backend = (*Router)(nil)
