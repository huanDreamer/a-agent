package server

import (
	"github.com/cloudwego/hertz/pkg/app"
	"go.uber.org/zap"
)

// Small zap helpers keep the API handlers readable and avoid repeating field
// construction at every call site.

func zapError(err error) zap.Field { return zap.Error(err) }

func zapString(k, v string) zap.Field { return zap.String(k, v) }

// zapRemote records the client address for an audit trail.
func zapRemote(c *app.RequestContext) zap.Field {
	return zap.String("remote", c.RemoteAddr().String())
}
