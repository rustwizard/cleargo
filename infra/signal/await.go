package signal

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog/log"
)

func Await(f func() error) {
	sigCh := make(chan os.Signal, 1)
	defer close(sigCh)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGABRT, syscall.SIGTERM, syscall.SIGQUIT)

	for range sigCh {
		err := f()
		if err != nil {
			log.Err(err).Msg("error while handling signal")
		}
	}
}

func WaitFirst() {
	sigCh := make(chan os.Signal, 1)
	defer close(sigCh)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGABRT, syscall.SIGTERM, syscall.SIGQUIT)

	<-sigCh
}
