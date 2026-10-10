package workivaprovider

import (
	"context"
	"errors"
	"testing"
)

type contractBackend struct {
	*fakeBackend
	contract ContentLiteralWriteContract
	err      error
	calls    int
}

func (b *contractBackend) ContentLiteralWriteContract(context.Context) (ContentLiteralWriteContract, error) {
	b.calls++
	return b.contract, b.err
}

func TestRouterLiteralWriterContractComesOnlyFromConfiguredWriter(t *testing.T) {
	reader := &contractBackend{fakeBackend: &fakeBackend{name: "reader"}, contract: ContentLiteralWriteContract{ProviderFamily: "reader-family", APIVersion: "reader-v1", Endpoint: "reader-endpoint", FormatPreservation: "preserves", Authority: "reader-attestation"}}
	writerWithoutProof := &fakeBackend{name: "writer"}
	router := NewRouter(&fakeBackend{name: "primary"}, WithReadBackend(reader), WithWriteBackend(writerWithoutProof))
	if _, err := router.ContentLiteralWriteContract(context.Background()); err == nil || reader.calls != 0 {
		t.Fatalf("reader contract was used as writer proof: err=%v reader calls=%d", err, reader.calls)
	}
	writer := &contractBackend{fakeBackend: &fakeBackend{name: "writer"}, contract: ContentLiteralWriteContract{ProviderFamily: "workiva", APIVersion: "signed-version", Endpoint: "configured-writer-endpoint", FormatPreservation: "preserves", Authority: "writer-attestation"}}
	router = NewRouter(&fakeBackend{name: "primary"}, WithReadBackend(reader), WithWriteBackend(writer))
	got, err := router.ContentLiteralWriteContract(context.Background())
	if err != nil || got != writer.contract || writer.calls != 1 || reader.calls != 0 {
		t.Fatalf("contract=%+v err=%v writer calls=%d reader calls=%d", got, err, writer.calls, reader.calls)
	}
}

func TestRouterRejectsUnknownOrIncompleteLiteralWriterContract(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contract ContentLiteralWriteContract
		err      error
	}{
		{name: "unknown preservation", contract: ContentLiteralWriteContract{ProviderFamily: "workiva", APIVersion: "v1", Endpoint: "endpoint", FormatPreservation: "unknown", Authority: "writer"}},
		{name: "missing authority", contract: ContentLiteralWriteContract{ProviderFamily: "workiva", APIVersion: "v1", Endpoint: "endpoint", FormatPreservation: "preserves"}},
		{name: "provider failure", contract: ContentLiteralWriteContract{ProviderFamily: "workiva", APIVersion: "v1", Endpoint: "endpoint", FormatPreservation: "preserves", Authority: "writer"}, err: errors.New("attestation unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := &contractBackend{fakeBackend: &fakeBackend{name: "writer"}, contract: tc.contract, err: tc.err}
			router := NewRouter(&fakeBackend{name: "primary"}, WithWriteBackend(writer))
			if _, err := router.ContentLiteralWriteContract(context.Background()); err == nil {
				t.Fatal("accepted absent or incomplete writer proof")
			}
		})
	}
}
