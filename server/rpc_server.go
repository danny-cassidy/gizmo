package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/NYTimes/logrotate"
	"github.com/go-kit/kit/metrics"
	"github.com/go-kit/kit/metrics/provider"

	"github.com/Sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// RPCServer is an experimental server that serves a gRPC server on one
// port and the same endpoints via JSON on another port.
type RPCServer struct {
	cfg *Config

	// exit chan for graceful shutdown
	exit chan chan error

	// server for handling RPC requests
	srvr *grpc.Server

	// mux for routing HTTP requests
	mux Router

	// tracks active requests
	monitor *ActivityMonitor

	mets         provider.Provider
	panicCounter metrics.Counter
}

// NewRPCServer will instantiate a new experimental RPCServer with the given config.
func NewRPCServer(cfg *Config) *RPCServer {
	if cfg == nil {
		cfg = &Config{}
	}
	mx := NewRouter(cfg)
	if cfg.NotFoundHandler != nil {
		mx.SetNotFoundHandler(cfg.NotFoundHandler)
	}
	mets := newMetricsProvider(cfg)
	return &RPCServer{
		cfg:          cfg,
		srvr:         makeGRPCServer(cfg),
		mux:          mx,
		exit:         make(chan chan error),
		monitor:      NewActivityMonitor(),
		mets:         mets,
		panicCounter: mets.NewCounter("panic", "counting any server panics"),
	}
}

// Register will attempt to register the given RPCService with the server.
// If any other types are passed, Register will panic.
func (r *RPCServer) Register(svc Service) error {
	rpcsvc, ok := svc.(RPCService)
	if !ok {
		Log.Fatalf("invalid service type for rpc server: %T", svc)
	}

	// register RPC
	desc, grpcSvc := rpcsvc.Service()
	r.srvr.RegisterService(desc, grpcSvc)
	// register endpoints
	for _, mthd := range desc.Methods {
		registerRPCMetrics(mthd.MethodName, r.mets)
	}

	// register HTTP
	// loop through json endpoints and register them
	prefix := svc.Prefix()
	// quick fix for backwards compatibility
	prefix = strings.TrimRight(prefix, "/")

	// register all context endpoints with our wrapper
	for path, epMethods := range rpcsvc.ContextEndpoints() {
		for method, ep := range epMethods {
			endpointName := metricName(prefix, path, method)
			// set the function handle and register it to metrics
			r.mux.Handle(method, prefix+path, Timed(CountedByStatusXX(
				func(ep ContextHandlerFunc, cs ContextService) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Body != nil {
							defer func() {
								if err := r.Body.Close(); err != nil {
									Log.Warn("unable to close request body: ", err)
								}
							}()
						}
						ctx := context.Background()
						rpcsvc.Middleware(ContextToHTTP(ctx, rpcsvc.ContextMiddleware(ep))).ServeHTTP(w, r)
					})
				}(ep, rpcsvc),
				endpointName+".STATUS-COUNT", r.mets),
				endpointName+".DURATION", r.mets),
			)
		}
	}

	// register all JSON context endpoints with our wrapper
	for path, epMethods := range rpcsvc.JSONEndpoints() {
		for method, ep := range epMethods {
			endpointName := metricName(prefix, path, method)
			// set the function handle and register it to metrics
			r.mux.Handle(method, prefix+path, Timed(CountedByStatusXX(
				rpcsvc.Middleware(ContextToHTTP(context.Background(),
					rpcsvc.ContextMiddleware(
						JSONContextToHTTP(rpcsvc.JSONMiddleware(ep)),
					),
				)),
				endpointName+".STATUS-COUNT", r.mets),
				endpointName+".DURATION", r.mets),
			)
		}
	}

	RegisterProfiler(r.cfg, r.mux)

	return nil
}

// Start start the RPC server.
func (r *RPCServer) Start() error {
	// setup RPC
	registerRPCAccessLogger(r.cfg)
	rl, err := net.Listen("tcp", fmt.Sprintf(":%d", r.cfg.RPCPort))
	if err != nil {
		return err
	}

	go func() {
		if err := r.srvr.Serve(rl); err != nil {
			Log.Error("encountered an error while serving RPC listener: ", err)
		}
	}()

	Log.Infof("RPC listening on %s", rl.Addr().String())

	// setup HTTP
	healthHandler := RegisterHealthHandler(r.cfg, r.monitor, r.mux)
	r.cfg.HealthCheckPath = healthHandler.Path()

	wrappedHandler, err := NewAccessLogMiddleware(r.cfg.RPCAccessLog, r)
	if err != nil {
		Log.Fatalf("unable to create http access log: %s", err)
	}

	srv := http.Server{
		Handler:        wrappedHandler,
		MaxHeaderBytes: maxHeaderBytes,
		ReadTimeout:    readTimeout,
		WriteTimeout:   writeTimeout,
	}
	var hl net.Listener
	hl, err = net.Listen("tcp", fmt.Sprintf(":%d", r.cfg.HTTPPort))
	if err != nil {
		return err
	}

	go func() {
		if err := srv.Serve(hl); err != nil {
			Log.Error("encountered an error while serving listener: ", err)
		}
	}()

	Log.Infof("HTTP listening on %s", hl.Addr().String())

	// join the LB
	go func() {
		exit := <-r.exit

		if err := healthHandler.Stop(); err != nil {
			Log.Warn("health check Stop returned with error: ", err)
		}

		r.srvr.Stop()
		exit <- hl.Close()
	}()

	return nil
}

// Stop will signal the RPC server to stop and block until it does.
func (r *RPCServer) Stop() error {
	ch := make(chan error)
	r.exit <- ch
	return <-ch
}

// ServeHTTP is RPCServer's hook for metrics and safely executing each request.
func (r *RPCServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	AddIPToContext(req)

	// only count non-LB requests
	if req.URL.Path != r.cfg.HealthCheckPath {
		r.monitor.CountRequest()
		defer r.monitor.UncountRequest()
	}

	r.safelyExecuteHTTPRequest(w, req)
}

// executeRequestSafely will prevent a panic in a request from bringing the server down.
func (r *RPCServer) safelyExecuteHTTPRequest(w http.ResponseWriter, req *http.Request) {
	defer func() {
		if x := recover(); x != nil {
			// register a panic'd request with our metrics
			r.panicCounter.Add(1)

			// log the panic for all the details later
			LogWithFields(req).Errorf("rpc server recovered from an HTTP panic\n%v: %v", x, string(debug.Stack()))

			// give the users our deepest regrets
			w.WriteHeader(http.StatusInternalServerError)
			if _, err := w.Write(UnexpectedServerError); err != nil {
				LogWithFields(req).Warn("unable to write response: ", err)
			}

		}
	}()

	// hand the request off to gorilla
	r.mux.ServeHTTP(w, req)
}

// LogRPCWithFields will feed any request context into a logrus Entry.
func LogRPCWithFields(log *logrus.Logger, ctx context.Context) *logrus.Entry {
	md, ok := metadata.FromContext(ctx)
	if !ok {
		return logrus.NewEntry(log)
	}
	return log.WithFields(MetadataToFields(md))
}

// MetadataToFields will accept all values from a metadata.MD and
// create logrus.Fields with the same set.
func MetadataToFields(md metadata.MD) logrus.Fields {
	f := logrus.Fields{}
	for k, v := range md {
		f[k] = v
	}
	return f
}

// MonitorRPCRequest should be deferred by any RPC method that would like to have
// metrics and access logging, participate in graceful shutdowns and safely recover from panics.
func MonitorRPCRequest() func(ctx context.Context, methodName string, err error) {
	start := time.Now()
	return func(ctx context.Context, methodName string, err error) {
		m := rpcEndpointMetrics["rpc."+methodName]

		if x := recover(); x != nil {
			// register a panic'd request with our metrics
			m.PanicCounter.Add(1)

			// log the panic for all the details later
			Log.Warningf("rpc server recovered from a panic\n%v: %v", x, string(debug.Stack()))

			// give the users our deepest regrets
			err = errors.New(string(UnexpectedServerError))
		}
		if err == nil {
			m.SuccessCounter.Add(1)
		} else {
			m.ErrorCounter.Add(1)
		}
		m.Timer.Observe(time.Since(start))

		if rpcAccessLog != nil {
			LogRPCWithFields(rpcAccessLog, ctx).WithFields(logrus.Fields{
				"name":     methodName,
				"duration": time.Since(start),
				"error":    err,
			}).Info("access")
		}
	}
}

var rpcEndpointMetrics = map[string]*rpcMetrics{}

type rpcMetrics struct {
	Timer          metrics.TimeHistogram
	SuccessCounter metrics.Counter
	ErrorCounter   metrics.Counter
	PanicCounter   metrics.Counter
}

func registerRPCMetrics(name string, mets provider.Provider) {
	name = "rpc." + name
	hist, err := mets.NewHistogram(name+".DURATION",
		fmt.Sprintf("tracking request duration for %q", name),
		0, 1500000, 4, // 0-15min time range
		50, 75, 90, 95, 99)
	if err != nil {
		panic(fmt.Sprint("invalid histogram settings: ", err))
	}
	rpcEndpointMetrics[name] = &rpcMetrics{
		Timer: metrics.NewTimeHistogram(time.Millisecond, hist),
		SuccessCounter: mets.NewCounter(name+".SUCCESS",
			fmt.Sprintf("counting successful repsonses for %s", name)),
		ErrorCounter: mets.NewCounter(name+".ERROR",
			fmt.Sprintf("counting error responses for %s", name)),
		PanicCounter: mets.NewCounter(name+".PANIC",
			fmt.Sprintf("counting paniced responses for %s", name)),
	}
}

// access logger
var rpcAccessLog *logrus.Logger

func registerRPCAccessLogger(cfg *Config) {
	// gRPC doesn't have a hook à la http.Handler-middleware
	// so some of this duplicates logic from config.NewAccessLogMiddleware
	if cfg.RPCAccessLog == nil {
		return
	}

	lf, err := logrotate.NewFile(*cfg.RPCAccessLog)
	if err != nil {
		Log.Fatalf("unable to access rpc access log file: %s", err)
	}

	rpcAccessLog = logrus.New()
	rpcAccessLog.Out = lf
}

func makeGRPCServer(cfg *Config) *grpc.Server {
	opts := make([]grpc.ServerOption, 0, 7)

	if cfg.RPCCodec != nil {
		opts = append(opts, grpc.CustomCodec(cfg.RPCCodec))
	}

	if cfg.RPCCompressor != nil {
		opts = append(opts, grpc.RPCCompressor(cfg.RPCCompressor))
	}

	if cfg.RPCCreds != nil {
		opts = append(opts, grpc.Creds(cfg.RPCCreds))
	}

	if cfg.RPCDecompressor != nil {
		opts = append(opts, grpc.RPCDecompressor(cfg.RPCDecompressor))
	}

	if cfg.RPCStreamInt != nil {
		opts = append(opts, grpc.StreamInterceptor(cfg.RPCStreamInt))
	}

	if cfg.RPCUnaryInt != nil {
		opts = append(opts, grpc.UnaryInterceptor(cfg.RPCUnaryInt))
	}

	if cfg.RPCMaxConcurrentStreams > 0 {
		opts = append(opts, grpc.MaxConcurrentStreams(cfg.RPCMaxConcurrentStreams))
	}

	return grpc.NewServer(opts...)
}
