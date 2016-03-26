package goa

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/net/context"
)

type (
	// Service is the data structure supporting goa services.
	// It provides methods for configuring a service and running it.
	// At the basic level a service consists of a set of controllers, each implementing a given
	// resource actions. goagen generates global functions - one per resource - that make it
	// possible to mount the corresponding controller onto a service. A service contains the
	// middleware, error handler, encoders and muxes shared by all its controllers. Setting up
	// a service might look like:
	//
	//	service := goa.New("my api")
	//	service.Use(SomeMiddleware())
	//	rc := NewResourceController()
	//	rc.Use(SomeOtherMiddleware())
	//	service.MountResourceController(service, rc)
	//	service.ListenAndServe(":80")
	//
	// where NewResourceController returns an object that implements the resource actions as
	// defined by the corresponding interface generated by goagen.
	Service struct {
		// Name of service used for logging, tracing etc.
		Name string
		// Mux is the service request mux
		Mux ServeMux
		// Context is the root context from which all request contexts are derived.
		// Set values in the root context prior to starting the server to make these values
		// available to all request handlers:
		//
		//	service.Context = context.WithValue(service.Context, key, value)
		//
		Context context.Context
		// Middleware chain
		Middleware []Middleware
		// Service-wide error handler
		ErrorHandler ErrorHandler

		cancel                context.CancelFunc
		decoderPools          map[string]*decoderPool // Registered decoders for the service
		encoderPools          map[string]*encoderPool // Registered encoders for the service
		encodableContentTypes []string                // List of contentTypes for response negotiation
	}

	// Controller provides the common state and behavior for generated controllers.
	Controller struct {
		Name         string          // Controller resource name
		Context      context.Context // Controller root context
		ErrorHandler ErrorHandler    // Controller specific error handler if any
		Middleware   []Middleware    // Controller specific middleware if any
	}

	// Handler defines the controller handler signatures.
	// If a controller handler returns an error then the service error handler is invoked
	// with the request context and the error. The error handler is responsible for writing the
	// HTTP response. See DefaultErrorHandler and TerseErrorHandler.
	Handler func(context.Context, http.ResponseWriter, *http.Request) error

	// ErrorHandler defines the service error handler signature.
	ErrorHandler func(context.Context, http.ResponseWriter, *http.Request, error)

	// Unmarshaler defines the request payload unmarshaler signatures.
	Unmarshaler func(context.Context, *http.Request) error

	// DecodeFunc is the function that initialize the unmarshaled payload from the request body.
	DecodeFunc func(context.Context, io.ReadCloser, interface{}) error
)

// New instantiates an service with the given name and default decoders/encoders.
func New(name string) *Service {
	stdlog := log.New(os.Stderr, "", log.LstdFlags)
	ctx := UseLogger(context.Background(), NewStdLogger(stdlog))
	ctx, cancel := context.WithCancel(ctx)
	return &Service{
		Name:         name,
		ErrorHandler: DefaultErrorHandler,
		Context:      ctx,
		Mux:          NewMux(),

		cancel:                cancel,
		decoderPools:          map[string]*decoderPool{},
		encoderPools:          map[string]*encoderPool{},
		encodableContentTypes: []string{},
	}
}

// CancelAll sends a cancel signals to all request handlers via the context.
// See https://godoc.org/golang.org/x/net/context for details on how to handle the signal.
func (service *Service) CancelAll() {
	service.cancel()
}

// Use adds a middleware to the service wide middleware chain.
// See NewMiddleware for wrapping goa and http handlers into goa middleware.
// goa comes with a set of commonly used middleware, see middleware.go.
// Controller specific middleware should be mounted using the Controller type Use method instead.
func (service *Service) Use(m Middleware) {
	service.Middleware = append(service.Middleware, m)
}

// UseLogger sets the logger used internally by the service and by Log.
func (service *Service) UseLogger(logger Logger) {
	service.Context = UseLogger(service.Context, logger)
}

// LogInfo logs the message and values at odd indeces using the keys at even indeces of the keyvals slice.
func (service *Service) LogInfo(msg string, keyvals ...interface{}) {
	LogInfo(service.Context, msg, keyvals...)
}

// LogError logs the error and values at odd indeces using the keys at even indeces of the keyvals slice.
func (service *Service) LogError(msg string, keyvals ...interface{}) {
	LogError(service.Context, msg, keyvals...)
}

// ListenAndServe starts a HTTP server and sets up a listener on the given host/port.
func (service *Service) ListenAndServe(addr string) error {
	service.LogInfo("listen", "transport", "http", "addr", addr)
	return http.ListenAndServe(addr, service.Mux)
}

// ListenAndServeTLS starts a HTTPS server and sets up a listener on the given host/port.
func (service *Service) ListenAndServeTLS(addr, certFile, keyFile string) error {
	service.LogInfo("listen", "transport", "https", "addr", addr)
	return http.ListenAndServeTLS(addr, certFile, keyFile, service.Mux)
}

// NewController returns a controller for the given resource. This method is mainly intended for
// use by the generated code. User code shouldn't have to call it directly.
func (service *Service) NewController(resName string) *Controller {
	ctx := context.WithValue(service.Context, serviceKey, service)
	return &Controller{
		Name:         resName,
		Middleware:   service.Middleware,
		ErrorHandler: service.ErrorHandler,
		Context:      context.WithValue(ctx, "ctrl", resName),
	}
}

// ServeFiles replies to the request with the contents of the named file or directory. The logic
// for what to do when the filename points to a file vs. a directory is the same as the standard
// http package ServeFile function. The path may end with a wildcard that matches the rest of the
// URL (e.g. *filepath). If it does the matching path is appended to filename to form the full file
// path, so:
// 	ServeFiles("/index.html", "/www/data/index.html")
// Returns the content of the file "/www/data/index.html" when requests are sent to "/index.html"
// and:
//	ServeFiles("/assets/*filepath", "/www/data/assets")
// returns the content of the file "/www/data/assets/x/y/z" when requests are sent to
// "/assets/x/y/z".
func (service *Service) ServeFiles(path, filename string) error {
	if strings.Contains(path, ":") {
		return fmt.Errorf("path may only include wildcards that match the entire end of the URL (e.g. *filepath)")
	}
	if _, err := os.Stat(filename); err != nil {
		return fmt.Errorf("ServeFiles: %s", err)
	}
	rel := filename
	if wd, err := os.Getwd(); err == nil {
		if abs, err := filepath.Abs(filename); err == nil {
			if r, err := filepath.Rel(wd, abs); err == nil {
				rel = r
			}
		}
	}
	service.LogInfo("mount file", "filepath", rel, "route", fmt.Sprintf("GET %s", path))
	ctrl := service.NewController("FileServer")
	var wc string
	if idx := strings.Index(path, "*"); idx > -1 && idx < len(path)-1 {
		wc = path[idx+1:]
	}
	handle := ctrl.MuxHandler("Serve", func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
		fullpath := filename
		r := ContextRequest(ctx)
		if len(wc) > 0 {
			if m, ok := r.Params[wc]; ok {
				fullpath = filepath.Join(fullpath, m[0])
			}
		}
		service.LogInfo("serve file", "filepath", fullpath, "path", r.URL.Path)
		http.ServeFile(ContextResponse(ctx), r.Request, fullpath)
		return nil
	}, nil)
	service.Mux.Handle("GET", path, handle)
	return nil
}

// Use adds a middleware to the controller.
// See NewMiddleware for wrapping goa and http handlers into goa middleware.
// goa comes with a set of commonly used middleware, see middleware.go.
func (ctrl *Controller) Use(m Middleware) {
	ctrl.Middleware = append(ctrl.Middleware, m)
}

// HandleError invokes the controller error handler or - if there isn't one - the service error
// handler.
func (ctrl *Controller) HandleError(ctx context.Context, rw http.ResponseWriter, req *http.Request, err error) {
	status := 500
	if e, ok := err.(*Error); ok {
		status = e.Status
	}
	go IncrCounter([]string{"goa", "handler", "error", strconv.Itoa(status)}, 1.0)
	if ctrl.ErrorHandler != nil {
		ctrl.ErrorHandler(ctx, rw, req, err)
		return
	}
	if h := ContextService(ctx).ErrorHandler; h != nil {
		h(ctx, rw, req, err)
	}
}

// MuxHandler wraps a request handler into a MuxHandler. The MuxHandler initializes the
// request context by loading the request state, invokes the handler and in case of error invokes
// the controller (if there is one) or Service error handler.
// This function is intended for the controller generated code. User code should not need to call
// it directly.
func (ctrl *Controller) MuxHandler(name string, hdlr Handler, unm Unmarshaler) MuxHandler {
	// Setup middleware outside of closure
	middleware := func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
		if !ContextResponse(ctx).Written() {
			if err := hdlr(ctx, rw, req); err != nil {
				ContextService(ctx).LogInfo("ERROR", "err", err)
				ctrl.HandleError(ctx, rw, req, err)
			}
		}
		return nil
	}
	chain := ctrl.Middleware
	ml := len(chain)
	for i := range chain {
		middleware = chain[ml-i-1](middleware)
	}
	baseCtx := LogWith(ctrl.Context, "action", name)
	return func(rw http.ResponseWriter, req *http.Request, params url.Values) {
		// Build context
		ctx := NewContext(baseCtx, rw, req, params)

		// Load body if any
		var err error
		if req.ContentLength > 0 && unm != nil {
			err = unm(ctx, req)
		}

		// Handle invalid payload
		handler := middleware
		if err != nil {
			handler = func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
				ctrl.ErrorHandler(ctx, rw, req, ErrInvalidEncoding(err))
				return nil
			}
			for i := range chain {
				handler = chain[ml-i-1](handler)
			}
		}

		// Invoke middleware chain, wrap writer to capture response status and length
		handler(ctx, ContextResponse(ctx), req)
	}
}

// DefaultErrorHandler returns a response with status 500 or the status specified in the error if
// an instance of HTTPStatusError.
// It writes the error message to the response body in both cases.
func DefaultErrorHandler(ctx context.Context, rw http.ResponseWriter, req *http.Request, e error) {
	status := 500
	var respBody interface{}
	switch err := e.(type) {
	case *Error:
		status = err.Status
		respBody = err
	default:
		respBody = e.Error()
	}
	if status == 500 {
		LogError(ctx, e.Error())
	}
	ContextResponse(ctx).Send(ctx, status, respBody)
}

// TerseErrorHandler behaves like DefaultErrorHandler except that it does not write to the response
// body for internal errors.
func TerseErrorHandler(ctx context.Context, rw http.ResponseWriter, req *http.Request, e error) {
	status := 500
	var respBody interface{}
	switch err := e.(type) {
	case *Error:
		status = err.Status
		if status != 500 {
			respBody = err
		}
	}
	if respBody == nil {
		respBody = "internal error"
	}
	if status == 500 {
		LogError(ctx, e.Error())
	}
	ContextResponse(ctx).Send(ctx, status, respBody)
}
