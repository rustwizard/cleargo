package cleargo

import (
	"context"

	"github.com/labstack/echo/v4"
)

type HTTPServer struct {
	addr string
	e    *echo.Echo
}

func NewHTTPServer(addr string) *HTTPServer {
	return &HTTPServer{
		e:    echo.New(),
		addr: addr,
	}
}

func (s *HTTPServer) Run() error {
	return s.e.Start(s.addr)
}

func (s *HTTPServer) Shutdown(ctx context.Context) error {
	return s.e.Shutdown(ctx)
}
