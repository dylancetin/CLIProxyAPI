package management

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
	"gopkg.in/yaml.v3"
)

func TestConfigUsagePricesPersistToConfigFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("config-version: 8\nserver: {port: 8317}\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	h := NewHandler(cfg, path, nil)
	h.setUsageSeriesStore(&fakeUsageStore{})

	want := `{"prices":{"claude-opus-5-5":{"input":15,"cache_read":1.5,"cache_write":18.75,"output":75}}}`
	rec := serveUsageTracker(t, h, http.MethodPut, "/usage/prices", want)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertJSONEqual(t, "PUT response", decodeUsageJSON(t, rec), want)
	assertJSONEqual(t, "GET after PUT", decodeUsageJSON(t, serveUsageTracker(t, h, http.MethodGet, "/usage/prices", "")), want)

	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var doc struct {
		Observability struct {
			Usage struct {
				Tracker struct {
					Prices map[string]map[string]float64 `yaml:"prices"`
				} `yaml:"tracker"`
			} `yaml:"usage"`
		} `yaml:"observability"`
	}
	if errParse := yaml.Unmarshal(data, &doc); errParse != nil {
		t.Fatalf("parse saved config: %v\n%s", errParse, data)
	}
	wantFile := map[string]map[string]float64{
		"claude-opus-5-5": {"input": 15, "cache-read": 1.5, "cache-write": 18.75, "output": 75},
	}
	if got := doc.Observability.Usage.Tracker.Prices; !reflect.DeepEqual(got, wantFile) {
		t.Fatalf("observability.usage.tracker.prices = %v, want %v\n%s", got, wantFile, data)
	}

	reloaded, errReload := config.LoadConfig(path)
	if errReload != nil {
		t.Fatal(errReload)
	}
	h.SetConfig(reloaded)
	assertJSONEqual(t, "GET after reload", decodeUsageJSON(t, serveUsageTracker(t, h, http.MethodGet, "/usage/prices", "")), want)

	fresh := NewHandler(reloaded, path, nil)
	fresh.setUsageSeriesStore(&fakeUsageStore{})
	assertJSONEqual(t, "GET from a new handler", decodeUsageJSON(t, serveUsageTracker(t, fresh, http.MethodGet, "/usage/prices", "")), want)
}

func TestConfigUsagePricesWithoutConfigFileReturns500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.UsageTracker.Prices = usagestore.Prices{"seed": {Input: 1}}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	h.setUsageSeriesStore(&fakeUsageStore{})

	rec := serveUsageTracker(t, h, http.MethodPut, "/usage/prices", `{"prices":{"m":{"input":1}}}`)
	if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != `{"error":"failed to save prices"}` {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertJSONEqual(t, "GET after failed PUT", decodeUsageJSON(t, serveUsageTracker(t, h, http.MethodGet, "/usage/prices", "")),
		`{"prices":{"seed":{"input":1,"cache_read":0,"cache_write":0,"output":0}}}`)
}

func TestConfigUsagePricesPutRemovesModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	seed := "config-version: 8\nserver: {port: 8317}\nobservability:\n  usage:\n    tracker:\n      prices:\n        a: {input: 1, output: 2}\n        b: {input: 3, output: 4}\n"
	if errWrite := os.WriteFile(path, []byte(seed), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	h := NewHandler(cfg, path, nil)
	h.setUsageSeriesStore(&fakeUsageStore{})

	steps := []struct {
		body string
		want usagestore.Prices
	}{
		{body: `{"prices":{"a":{"input":1,"output":2}}}`, want: usagestore.Prices{"a": {Input: 1, Output: 2}}},
		{body: `{"prices":{}}`, want: usagestore.Prices{}},
	}
	for _, step := range steps {
		if rec := serveUsageTracker(t, h, http.MethodPut, "/usage/prices", step.body); rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: status=%d body=%s", step.body, rec.Code, rec.Body.String())
		}
		reloaded, errReload := config.LoadConfig(path)
		if errReload != nil {
			t.Fatal(errReload)
		}
		got := usagestore.Prices(reloaded.UsageTracker.Prices)
		if len(got) != len(step.want) || (len(step.want) > 0 && !reflect.DeepEqual(got, step.want)) {
			data, _ := os.ReadFile(path)
			t.Fatalf("after PUT %s reloaded prices = %+v, want %+v\n%s", step.body, got, step.want, data)
		}
	}
}
