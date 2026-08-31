//go:build windows || darwin

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	wailsassetserver "github.com/wailsapp/wails/v2/pkg/assetserver"
)

type testLogger struct{}

func (testLogger) Debug(string, ...interface{}) {}
func (testLogger) Error(string, ...interface{}) {}

func TestAssetServerRoutesRootToAppHandler(t *testing.T) {
	wantBody := "mosaic app handler"
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(wantBody))
	})

	options := newAssetServerOptions(handler)
	if options.Assets != nil {
		t.Fatal("desktop assets must be nil so Wails forwards GET requests to the app handler")
	}

	assetHandler, err := wailsassetserver.NewAssetHandler(*options, testLogger{})
	if err != nil {
		t.Fatalf("create Wails asset handler: %v", err)
	}

	recorder := httptest.NewRecorder()
	assetHandler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://wails.localhost/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Body.String(); got != wantBody {
		t.Fatalf("body = %q, want %q", got, wantBody)
	}
}
