package cleargo

import (
	"reflect"
	"testing"
)

func TestNewHTTPServer(t *testing.T) {
	type args struct {
		addr string
	}
	tests := []struct {
		name string
		args args
		want *HTTPServer
	}{
		{
			name: "success",
			args: args{addr: "127.0.0.1:61000"},
			want: &HTTPServer{
				addr: "127.0.0.1:61000",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewHTTPServer(tt.args.addr); !reflect.DeepEqual(got.addr, tt.want.addr) {
				t.Errorf("NewHTTPServer() = %v, want %v", got, tt.want)
			}
		})
	}
}
