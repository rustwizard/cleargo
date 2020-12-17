package pg_test

import (
	"net"
	"testing"
	"time"

	"github.com/jackc/pgmock"
	"github.com/jackc/pgproto3/v2"
	"github.com/rustwizard/cleargo/db/pg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConnect(t *testing.T) {
	script := &pgmock.Script{
		Steps: pgmock.AcceptUnauthenticatedConnRequestSteps(),
	}
	script.Steps = append(script.Steps, pgmock.ExpectMessage(&pgproto3.Terminate{}))
	ln, err := net.Listen("tcp4", "127.0.0.1:65432")
	require.NoError(t, err)
	defer ln.Close()

	serverErrChan := make(chan error, 1)
	go func() {
		defer close(serverErrChan)

		conn, err := ln.Accept()
		if err != nil {
			serverErrChan <- err
			return
		}
		defer conn.Close()

		err = conn.SetDeadline(time.Now().Add(time.Second))
		if err != nil {
			serverErrChan <- err
			return
		}

		err = script.Run(pgproto3.NewBackend(pgproto3.NewChunkReader(conn), conn))
		if err != nil {
			serverErrChan <- err
			return
		}
	}()

	db := pg.NewDB()
	err = db.Connect(&pg.Config{
		Host:         "127.0.0.1",
		Port:         65432,
		User:         "",
		Password:     "",
		DatabaseName: "",
		Schema:       "",
		SSL:          "disable",
		MaxPoolSize:  10,
	})
	require.NoError(t, err)
	db.Close()
	assert.NoError(t, <-serverErrChan)
}
