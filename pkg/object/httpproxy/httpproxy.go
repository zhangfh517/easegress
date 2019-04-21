package httpproxy

import (
	"fmt"
	"net/http"

	"github.com/megaease/easegateway/pkg/context"
	"github.com/megaease/easegateway/pkg/plugin/adaptor"
	"github.com/megaease/easegateway/pkg/plugin/backend"
	"github.com/megaease/easegateway/pkg/plugin/candidatebackend"
	"github.com/megaease/easegateway/pkg/plugin/circuitbreaker"
	"github.com/megaease/easegateway/pkg/plugin/compression"
	"github.com/megaease/easegateway/pkg/plugin/mirrorbackend"
	"github.com/megaease/easegateway/pkg/plugin/ratelimiter"
	"github.com/megaease/easegateway/pkg/plugin/validator"
	"github.com/megaease/easegateway/pkg/registry"

	metrics "github.com/rcrowley/go-metrics"
)

func init() {
	registry.Register(Kind, DefaultSpec)
}

const (
	// Kind is HTTPProxy kind.
	Kind = "HTTPProxy"
)

type (
	// HTTPProxy is Object HTTPProxy.
	HTTPProxy struct {
		spec  *Spec
		rate1 metrics.EWMA

		fallback *proxyFallback

		validator        *validator.Validator
		rateLimiter      *ratelimiter.RateLimiter
		circuitBreaker   *circuitbreaker.CircuitBreaker
		adaptor          *adaptor.Adaptor
		mirrorBackend    *mirrorbackend.MirrorBackend
		candidateBackend *candidatebackend.CandidateBackend
		backend          *backend.Backend
		compression      *compression.Compression
	}

	// Spec describes the HTTPProxy.
	Spec struct {
		registry.MetaSpec `yaml:",inline"`
		Server            string `yaml:"server" v:"required"`

		Fallback *proxyFallbackSpec `yaml:"fallback,omitempty"`

		Validator        *validator.Spec        `yaml:"validator,omitempty"`
		RateLimiter      *ratelimiter.Spec      `yaml:"rateLimiter,omitempty"`
		CircuitBreaker   *circuitbreaker.Spec   `yaml:"circuitBreaker,omitempty"`
		Adaptor          *adaptor.Spec          `yaml:"adaptor,omitempty"`
		MirrorBackend    *mirrorbackend.Spec    `yaml:"mirrorBackend,omitempty"`
		CandidateBackend *candidatebackend.Spec `yaml:"candidateBackend,omitempty"`
		Backend          *backend.Spec          `yaml:"backend" v:"required"`
		Compression      *compression.Spec      `yaml:"compression,omitempty"`
	}
)

// New creates an HTTPProxy.
func New(spec *Spec, runtime *Runtime) *HTTPProxy {
	runtime.reload(spec)

	hp := &HTTPProxy{
		spec:  spec,
		rate1: runtime.rate1,
	}

	if spec.Fallback != nil {
		hp.fallback = newProxyFallback(spec.Fallback, runtime.fallback)
	}

	if spec.Validator != nil {
		hp.validator = validator.New(spec.Validator, runtime.validator)
	}
	if spec.RateLimiter != nil {
		hp.rateLimiter = ratelimiter.New(spec.RateLimiter, runtime.rateLimiter)
	}
	if spec.CircuitBreaker != nil {
		hp.circuitBreaker = circuitbreaker.New(spec.CircuitBreaker, runtime.circuitBreaker)
	}
	if spec.Adaptor != nil {
		hp.adaptor = adaptor.New(spec.Adaptor, runtime.adaptor)
	}
	if spec.MirrorBackend != nil {
		hp.mirrorBackend = mirrorbackend.New(spec.MirrorBackend, runtime.mirrorBackend)
	}

	if spec.CandidateBackend != nil {
		hp.candidateBackend = candidatebackend.New(spec.CandidateBackend, runtime.candidateBackend)
	}

	hp.backend = backend.New(spec.Backend, runtime.backend)

	if spec.Compression != nil {
		hp.compression = compression.New(spec.Compression, runtime.compression)
		if hp.candidateBackend != nil {
			hp.candidateBackend.OnResponseGot(hp.compression.Compress)
		}
		hp.backend.OnResponseGot(hp.compression.Compress)
	}

	return hp
}

// DefaultSpec returns HTTPProxy default spec.
func DefaultSpec() registry.Spec {
	// FIXME: Do we need provide default spec if spec has some empty subspec.
	return &Spec{}
}

// Handle handles all incoming traffic.
func (hp *HTTPProxy) Handle(ctx context.HTTPContext) {
	defer ctx.OnFinish(func() {
		hp.rate1.Update(1)
	})

	hp.preHandle(ctx)
	if ctx.Cancelled() {
		return
	}

	hp.handle(ctx)
	if ctx.Cancelled() {
		return
	}

	hp.postHandle(ctx)
}

func (hp *HTTPProxy) preHandle(ctx context.HTTPContext) {
	w := ctx.Response()

	if hp.validator != nil {
		err := hp.validator.Validate(ctx)
		if err != nil {
			// NOTICE: No fallback for invalid traffic.
			w.SetStatusCode(http.StatusBadRequest)
			ctx.Cancel(fmt.Errorf("validate failed: %v", err))
			return
		}
	}

	if hp.rateLimiter != nil {
		err := hp.rateLimiter.Limit(ctx)
		if err != nil {
			w.SetStatusCode(http.StatusTooManyRequests)
			// NOTICE: Return regardless of result.
			hp.tryFallback(ctx, fallbackPluginRateLimiter, err)
			return
		}
	}

	if hp.adaptor != nil {
		hp.adaptor.AdaptRequest(ctx)
	}

	if hp.mirrorBackend != nil {
		hp.mirrorBackend.Handle(ctx)
	}
}

func (hp *HTTPProxy) handle(ctx context.HTTPContext) {
	pt, handler := fallbackPluginBackend, hp.backend.Handle
	if hp.candidateBackend != nil && hp.candidateBackend.Filter(ctx) {
		pt, handler = fallbackPluginCandidateBackend, hp.candidateBackend.Handle
	}

	if hp.circuitBreaker != nil {
		err := hp.circuitBreaker.Protect(ctx, handler)
		if err != nil {
			ctx.Response().SetStatusCode(http.StatusServiceUnavailable)
			hp.tryFallback(ctx, fallbackPluginCircuitBreaker, err)
		} else {
			hp.tryFallback(ctx, pt, nil /*error*/)
		}
	} else {
		handler(ctx)
		hp.tryFallback(ctx, pt, nil /*error*/)
	}

}

func (hp *HTTPProxy) postHandle(ctx context.HTTPContext) {
	if hp.adaptor != nil {
		hp.adaptor.AdaptResponse(ctx)
	}
}

func (hp *HTTPProxy) tryFallback(ctx context.HTTPContext, pt fallbackPlugin, err error) {
	if hp.fallback != nil {
		hp.fallback.tryFallback(ctx, pt, err)
	} else if err != nil {
		ctx.Cancel(err)
	}
}

// Close closes HTTPProxy.
func (hp *HTTPProxy) Close() {
	if hp.fallback != nil {
		hp.fallback.close()
	}
	if hp.validator != nil {
		hp.validator.Close()
	}
	if hp.rateLimiter != nil {
		hp.rateLimiter.Close()
	}
	if hp.circuitBreaker != nil {
		hp.circuitBreaker.Close()
	}
	if hp.adaptor != nil {
		hp.adaptor.Close()
	}
	if hp.mirrorBackend != nil {
		hp.mirrorBackend.Close()
	}
	if hp.candidateBackend != nil {
		hp.candidateBackend.Close()
	}

	hp.backend.Close()

	if hp.compression != nil {
		hp.compression.Close()
	}
}
