package workivaprovider

import (
	"context"
	"errors"
	"strings"
)

// ContentLiteralWriteContract is explicit evidence about the configured
// mutation route. Read metadata cannot establish which backend will write.
type ContentLiteralWriteContract struct {
	ProviderFamily     string
	APIVersion         string
	Endpoint           string
	FormatPreservation string
	Authority          string
}

// ContentLiteralWriteContractProvider is implemented by a writer only when
// that writer can attest to its exact literal-write route and guarantees.
type ContentLiteralWriteContractProvider interface {
	ContentLiteralWriteContract(context.Context) (ContentLiteralWriteContract, error)
}

// ContentLiteralWriteContract delegates only to the configured writer. It
// never falls back to the metadata reader or primary backend.
func (r *Router) ContentLiteralWriteContract(ctx context.Context) (ContentLiteralWriteContract, error) {
	if r == nil || r.writer == nil || isNilInterface(r.writer) {
		return ContentLiteralWriteContract{}, errors.New("workivaprovider: literal writer contract is unavailable")
	}
	if _, recursive := r.writer.(*Router); recursive {
		return ContentLiteralWriteContract{}, errors.New("workivaprovider: recursive literal writer contract rejected")
	}
	provider, ok := r.writer.(ContentLiteralWriteContractProvider)
	if !ok {
		return ContentLiteralWriteContract{}, errors.New("workivaprovider: configured writer has no literal-write contract")
	}
	contract, err := provider.ContentLiteralWriteContract(ctx)
	if err != nil {
		return ContentLiteralWriteContract{}, err
	}
	if strings.TrimSpace(contract.ProviderFamily) == "" || strings.TrimSpace(contract.APIVersion) == "" || strings.TrimSpace(contract.Endpoint) == "" || strings.TrimSpace(contract.Authority) == "" || contract.FormatPreservation != "preserves" {
		return ContentLiteralWriteContract{}, errors.New("workivaprovider: configured writer literal-write contract is unknown or incomplete")
	}
	return contract, nil
}
