package server

import (
	"github.com/cloudwego/hertz/pkg/app"
	"go.uber.org/zap"
)

// Small zap helpers keep the API handlers readable and avoid repeating field
// construction at every call site.

func zapError(err error) zap.Field { return zap.Error(err) }

func zapString(k, v string) zap.Field { return zap.String(k, v) }

func zapBool(k string, v bool) zap.Field { return zap.Bool(k, v) }

// zapInt is zap.Int under a name that matches its siblings above.
func zapInt(k string, v int) zap.Field { return zap.Int(k, v) }

// zapInt64 is zap.Int64, for the sizes (bytes, caps) the artifact store logs.
func zapInt64(k string, v int64) zap.Field { return zap.Int64(k, v) }

// zapRemote records the client address for an audit trail.
func zapRemote(c *app.RequestContext) zap.Field {
	return zap.String("remote", c.RemoteAddr().String())
}
