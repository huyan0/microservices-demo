// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	//"go.opentelemetry.io/otel/api/correlation"
	"github.com/newrelic/newrelic-telemetry-sdk-go/telemetry"
	"github.com/newrelic/opentelemetry-exporter-go/newrelic"
	"go.opentelemetry.io/otel/api/global"
	"go.opentelemetry.io/otel/api/key"
	"go.opentelemetry.io/otel/api/metric"
	"go.opentelemetry.io/otel/api/unit"
	"go.opentelemetry.io/otel/exporters/trace/stdout"
	"go.opentelemetry.io/otel/plugin/grpctrace"
	"go.opentelemetry.io/otel/sdk/metric/batcher/ungrouped"
	"go.opentelemetry.io/otel/sdk/metric/controller/push"
	"go.opentelemetry.io/otel/sdk/metric/selector/simple"
	"go.opentelemetry.io/otel/sdk/trace"
)

const (
	port            = "8080"
	defaultCurrency = "USD"
	cookieMaxAge    = 60 * 60 * 48

	cookiePrefix    = "shop_"
	cookieSessionID = cookiePrefix + "session-id"
	cookieCurrency  = cookiePrefix + "currency"
)

var (
	whitelistedCurrencies = map[string]bool{
		"USD": true,
		"EUR": true,
		"CAD": true,
		"JPY": true,
		"GBP": true,
		"TRY": true}
)

type ctxKeySessionID struct{}

type frontendServer struct {
	productCatalogSvcAddr string
	productCatalogSvcConn *grpc.ClientConn

	currencySvcAddr string
	currencySvcConn *grpc.ClientConn

	cartSvcAddr string
	cartSvcConn *grpc.ClientConn

	recommendationSvcAddr string
	recommendationSvcConn *grpc.ClientConn

	checkoutSvcAddr string
	checkoutSvcConn *grpc.ClientConn

	shippingSvcAddr string
	shippingSvcConn *grpc.ClientConn

	adSvcAddr string
	adSvcConn *grpc.ClientConn
}

func main() {
	ctx := context.Background()
	log := logrus.New()
	log.Level = logrus.DebugLevel
	log.Formatter = &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "timestamp",
			logrus.FieldKeyLevel: "severity",
			logrus.FieldKeyMsg:   "message",
		},
		TimestampFormat: time.RFC3339Nano,
	}
	log.Out = os.Stdout
	if os.Getenv("DISABLE_TRACING") == "" {
		log.Info("Tracing enabled.")
		initTracing(log)
	} else {
		log.Info("Tracing disabled.")
	}

	srvPort := port
	if os.Getenv("PORT") != "" {
		srvPort = os.Getenv("PORT")
	}
	addr := os.Getenv("LISTEN_ADDR")
	svc := new(frontendServer)
	mustMapEnv(&svc.productCatalogSvcAddr, "PRODUCT_CATALOG_SERVICE_ADDR")
	mustMapEnv(&svc.currencySvcAddr, "CURRENCY_SERVICE_ADDR")
	mustMapEnv(&svc.cartSvcAddr, "CART_SERVICE_ADDR")
	mustMapEnv(&svc.recommendationSvcAddr, "RECOMMENDATION_SERVICE_ADDR")
	mustMapEnv(&svc.checkoutSvcAddr, "CHECKOUT_SERVICE_ADDR")
	mustMapEnv(&svc.shippingSvcAddr, "SHIPPING_SERVICE_ADDR")
	mustMapEnv(&svc.adSvcAddr, "AD_SERVICE_ADDR")

	mustConnGRPC(ctx, &svc.currencySvcConn, svc.currencySvcAddr)
	mustConnGRPC(ctx, &svc.productCatalogSvcConn, svc.productCatalogSvcAddr)
	mustConnGRPC(ctx, &svc.cartSvcConn, svc.cartSvcAddr)
	mustConnGRPC(ctx, &svc.recommendationSvcConn, svc.recommendationSvcAddr)
	mustConnGRPC(ctx, &svc.shippingSvcConn, svc.shippingSvcAddr)
	mustConnGRPC(ctx, &svc.checkoutSvcConn, svc.checkoutSvcAddr)
	mustConnGRPC(ctx, &svc.adSvcConn, svc.adSvcAddr)

	r := mux.NewRouter()
	r.HandleFunc("/", svc.homeHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/product/{id}", svc.productHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/cart", svc.viewCartHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/cart", svc.addToCartHandler).Methods(http.MethodPost)
	r.HandleFunc("/cart/empty", svc.emptyCartHandler).Methods(http.MethodPost)
	r.HandleFunc("/setCurrency", svc.setCurrencyHandler).Methods(http.MethodPost)
	r.HandleFunc("/logout", svc.logoutHandler).Methods(http.MethodGet)
	r.HandleFunc("/cart/checkout", svc.placeOrderHandler).Methods(http.MethodPost)
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("./static/"))))
	r.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "User-agent: *\nDisallow: /") })
	r.HandleFunc("/_healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })

	meter := global.MeterProvider().Meter("Frontend")

	var handler http.Handler = r
	hostname, err := os.Hostname()
	if err != nil {
		// TODO: handle this properly
	}
	hostKey := key.New("host").String(hostname)
	requestCount, err := meter.NewInt64Counter(
		"http_request_count",
		metric.WithUnit(unit.Dimensionless),
		metric.WithDescription("Number of incoming requests"),
	)
	if err != nil {
		// TODO: handle this properly
	}
	requestLatency, err := meter.NewInt64Measure(
		"http_request_latency",
		metric.WithUnit(unit.Milliseconds),
		metric.WithDescription("Time spent responding to a request"),
	)
	if err != nil {
		// TODO: handle this properly
	}
	errorCount, err := meter.NewInt64Counter(
		"http_error_count",
		metric.WithUnit(unit.Dimensionless),
		metric.WithDescription("Number of errored requests"),
	)
	if err != nil {
		// TODO: handle this properly
	}
	handler = &telemetryHandler{
		requestCount:   requestCount.Bind(hostKey),
		requestLatency: requestLatency.Bind(hostKey),
		errorCount:     errorCount.Bind(hostKey),
		next:           handler,
	}
	handler = &logHandler{log: log, next: handler} // add logging
	handler = ensureSessionID(handler)             // add session ID
	log.Infof("starting server on " + addr + ":" + srvPort)
	log.Fatal(http.ListenAndServe(addr+":"+srvPort, handler))
}

func checkEnvVar(s string) bool {
	return s != "" && s != "<no value>"
}

var pusher *push.Controller

func initTracing(log logrus.FieldLogger) {
	// Create stdout exporter to be able to retrieve
	// the collected spans.
	api_key := os.Getenv("NEW_RELIC_API_KEY")
	if checkEnvVar(api_key) {
		log.Info("Using New Relic API KEY: " + api_key)
		exporter, err := newrelic.NewExporter(
			"Frontend",
			api_key,
			func(cfg *telemetry.Config) {
				metricURL := os.Getenv("NEW_RELIC_METRIC_URL")
				if checkEnvVar(metricURL) {
					log.Info("Setting metric export endpoint to " + metricURL)
					cfg.MetricsURLOverride = metricURL
				}
				traceURL := os.Getenv("NEW_RELIC_TRACE_URL")
				if checkEnvVar(traceURL) {
					log.Info("Setting trace export endpoint to " + traceURL)
					cfg.SpansURLOverride = traceURL
				}
			},
		)
		if err != nil {
			log.Fatal(err)
		}

		tp, err := trace.NewProvider(trace.WithSyncer(exporter))
		if err != nil {
			log.Fatal(err)
		}
		// TODO: enable these piecemeal based on available urls
		global.SetTraceProvider(tp)

		selector := simple.NewWithExactMeasure()
		batcher := ungrouped.New(selector, true)
		pusher = push.New(batcher, exporter, time.Second)
		pusher.Start()
		global.SetMeterProvider(pusher)
	} else {
		log.Info("No New Relic API key found, defaulting to stdout exporter")
		// Create stdout exporter to be able to retrieve
		// the collected spans.
		exporter, err := stdout.NewExporter(stdout.Options{PrettyPrint: true})
		if err != nil {
			log.Fatal(err)
		}

		// For the demonstration, use sdktrace.AlwaysSample sampler to sample all traces.
		// In a production application, use sdktrace.ProbabilitySampler with a desired probability.
		tp, err := trace.NewProvider(trace.WithConfig(trace.Config{DefaultSampler: trace.AlwaysSample()}),
			trace.WithSyncer(exporter))
		if err != nil {
			log.Fatal(err)
		}
		global.SetTraceProvider(tp)
		// TODO: use stdout exporter
	}
}

func mustMapEnv(target *string, envKey string) {
	v := os.Getenv(envKey)
	if v == "" {
		panic(fmt.Sprintf("environment variable %q not set", envKey))
	}
	*target = v
}

func mustConnGRPC(ctx context.Context, conn **grpc.ClientConn, addr string) {
	var err error
	*conn, err = grpc.DialContext(ctx, addr,
		grpc.WithInsecure(),
		grpc.WithTimeout(time.Second*3),
		grpc.WithUnaryInterceptor(grpctrace.UnaryClientInterceptor(global.Tracer("Frontend"))),
		grpc.WithStreamInterceptor(grpctrace.StreamClientInterceptor(global.Tracer("Frontend"))),
	)
	if err != nil {
		panic(errors.Wrapf(err, "grpc: failed to connect %s", addr))
	}
}
