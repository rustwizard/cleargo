package cleargo

import (
	"context"

	"github.com/labstack/echo/v4"
)

type HTTPServer struct {
	*echo.Echo
	addr string
}

func NewHTTPServer(addr string) *HTTPServer {
	return &HTTPServer{
		Echo: echo.New(),
		addr: addr,
	}
}

func (s *HTTPServer) Run() error {
	return s.Start(s.addr)
}

func (s *HTTPServer) Shutdown(ctx context.Context) error {
	return s.Echo.Shutdown(ctx)
}
