package main

import (
	"bufio"
	"log"
	"net/http"
	"os"
	"os/signal"

	"lukechampine.com/us/renter/renterutil"

	"github.com/pkg/errors"
	"lukechampine.com/us/renter"
)

func serve(contractDir, metaDir, addr string) error {
	contracts, err := renter.LoadContracts(contractDir)
	if err != nil {
		return errors.Wrap(err, "could not load contracts")
	}
	defer contracts.Close()

	c := makeLimitedClient()
	pfs, err := renterutil.NewFileSystem(metaDir, contracts, c)
	if err != nil {
		return errors.Wrap(err, "could not connect to hosts")
	}
	srv := &http.Server{
		Addr:    addr,
		Handler: http.FileServer(&httpFS{pfs}),
	}
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt)
		<-sigChan
		log.Println("Stopping server...")
		srv.Close()
		pfs.Close()
	}()
	log.Printf("Listening on %v...", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// A bufferedFile wraps a renterutil.PseudoFile in a bufio.Reader for better
// performance.
type bufferedFile struct {
	renterutil.PseudoFile
	br *bufio.Reader
}

func (f bufferedFile) Read(p []byte) (int, error) { return f.br.Read(p) }

type httpFS struct {
	pfs *renterutil.PseudoFS
}

func (hfs *httpFS) Open(name string) (http.File, error) {
	pf, err := hfs.pfs.Open(name)
	if err != nil {
		return nil, err
	}
	return bufferedFile{
		PseudoFile: pf,
		br:         bufio.NewReaderSize(pf, 1<<20), // 1 MiB
	}, nil
}
