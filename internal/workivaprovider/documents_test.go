package workivaprovider

import (
	"net/url"
	"testing"

	"github.com/dantalabs/northern-lights/internal/workiva"
)

var _ DocumentWriter = (*workiva.Client)(nil)

func TestDocumentWriterResolvesPrimary(t *testing.T) {
	base, _ := url.Parse("https://api.eu.wdesk.com")
	client := workiva.NewDemoClient(base)
	for _, backend := range []Backend{client, NewRouter(client)} {
		writer, ok := DocumentWriterFor(backend)
		if !ok || writer != client {
			t.Fatal("document writer did not resolve to primary client")
		}
	}
	if _, ok := DocumentWriterFor(nil); ok {
		t.Fatal("nil backend exposes document writes")
	}
}
