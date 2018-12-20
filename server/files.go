// Copyright 2018 The Moov Authors
// Use of this source code is governed by an Apache License
// license that can be found in the LICENSE file.

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/moov-io/ach"
	"github.com/moov-io/base"
	moovhttp "github.com/moov-io/base/http"

	"github.com/go-kit/kit/endpoint"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/metrics/prometheus"
	"github.com/gorilla/mux"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
)

var (
	filesCreated = prometheus.NewCounterFrom(stdprometheus.CounterOpts{
		Name: "ach_files_created",
		Help: "The number of ACH files created",
	}, []string{"destination", "origin"})

	filesDeleted = prometheus.NewCounterFrom(stdprometheus.CounterOpts{
		Name: "ach_files_deleted",
		Help: "The number of ACH files deleted",
	}, nil)
)

type createFileRequest struct {
	File *ach.File

	requestId string
}

type createFileResponse struct {
	ID  string `json:"id"`
	Err error  `json:"error"`
}

func (r createFileResponse) error() error { return r.Err }

func createFileEndpoint(s Service, r Repository, logger log.Logger) endpoint.Endpoint {
	return func(_ context.Context, request interface{}) (interface{}, error) {
		req := request.(createFileRequest)

		// record a metric for files created
		if req.File != nil && req.File.Header.ImmediateDestination != "" && req.File.Header.ImmediateOrigin != "" {
			filesCreated.With("destination", req.File.Header.ImmediateDestination, "origin", req.File.Header.ImmediateOrigin).Add(1)
		}

		// Create a random file ID if none was provided
		if req.File.ID == "" {
			req.File.ID = NextID()
		}

		// Reject files with a batch that was supposed to be posted in the past.
		if err := fileHasOldBatches(req.File); err != nil {
			return createFileResponse{
				ID:  req.File.ID,
				Err: err,
			}, err
		}

		err := r.StoreFile(req.File)
		if req.requestId != "" && logger != nil {
			logger.Log("files", "createFile", "requestId", req.requestId, "error", err)
		}

		return createFileResponse{
			ID:  req.File.ID,
			Err: err,
		}, nil
	}
}

func fileHasOldBatches(file *ach.File) error {
	if file == nil {
		return errors.New("no ACH file provided")
	}

	// Get a time.Time for the start of today
	now := base.Now()
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())

	for i := range file.Batches {
		header := file.Batches[i].GetHeader()
		if header == nil {
			continue
		}
		if header.EffectiveEntryDate.Before(today) {
			return fmt.Errorf("file=%s batch=%s has EffectiveEntryDate before today: %v", file.ID, file.Batches[i].ID(), header.EffectiveEntryDate)
		}
	}
	for i := range file.IATBatches {
		header := file.Batches[i].GetHeader()
		if header == nil {
			continue
		}
		if header.EffectiveEntryDate.Before(today) {
			return fmt.Errorf("file=%s IATBatch=%s has EffectiveEntryDate before today: %v", file.ID, file.Batches[i].ID(), header.EffectiveEntryDate)
		}
	}
	return nil
}

func decodeCreateFileRequest(_ context.Context, request *http.Request) (interface{}, error) {
	var r io.Reader
	var req createFileRequest

	req.requestId = moovhttp.GetRequestId(request)

	// Sets default values
	req.File = ach.NewFile()
	bs, err := ioutil.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}

	h := request.Header.Get("Content-Type")
	if strings.Contains(h, "application/json") {
		// Read body as ACH file in JSON
		f, err := ach.FileFromJSON(bs)
		if err != nil {
			return nil, err
		}
		req.File = f
	} else {
		// Attempt parsing body as an ACH File
		r = bytes.NewReader(bs)
		f, err := ach.NewReader(r).Read()
		if err != nil {
			return nil, err
		}
		req.File = &f
	}
	return req, nil
}

type getFilesRequest struct {
	requestId string
}

type getFilesResponse struct {
	Files []*ach.File `json:"files"`
	Err   error       `json:"error"`
}

func (r getFilesResponse) count() int { return len(r.Files) }

func (r getFilesResponse) error() error { return r.Err }

func getFilesEndpoint(s Service) endpoint.Endpoint {
	return func(_ context.Context, _ interface{}) (interface{}, error) {
		return getFilesResponse{
			Files: s.GetFiles(),
			Err:   nil,
		}, nil
	}
}

func decodeGetFilesRequest(_ context.Context, r *http.Request) (interface{}, error) {
	return getFilesRequest{
		requestId: moovhttp.GetRequestId(r),
	}, nil
}

type getFileRequest struct {
	ID string

	requestId string
}

type getFileResponse struct {
	File *ach.File `json:"file"`
	Err  error     `json:"error"`
}

func (r getFileResponse) error() error { return r.Err }

func getFileEndpoint(s Service, logger log.Logger) endpoint.Endpoint {
	return func(_ context.Context, request interface{}) (interface{}, error) {
		req := request.(getFileRequest)
		f, err := s.GetFile(req.ID)

		if req.requestId != "" && logger != nil {
			logger.Log("files", "getFile", "requestId", req.requestId, "error", err)
		}

		return getFileResponse{
			File: f,
			Err:  err,
		}, nil
	}
}

func decodeGetFileRequest(_ context.Context, r *http.Request) (interface{}, error) {
	vars := mux.Vars(r)
	id, ok := vars["id"]
	if !ok {
		return nil, ErrBadRouting
	}
	return getFileRequest{
		ID:        id,
		requestId: moovhttp.GetRequestId(r),
	}, nil
}

type deleteFileRequest struct {
	ID string

	requestId string
}

type deleteFileResponse struct {
	Err error `json:"err"`
}

func (r deleteFileResponse) error() error { return r.Err }

func deleteFileEndpoint(s Service, logger log.Logger) endpoint.Endpoint {
	return func(_ context.Context, request interface{}) (interface{}, error) {
		req := request.(deleteFileRequest)
		filesDeleted.Add(1)

		err := s.DeleteFile(req.ID)

		if req.requestId != "" && logger != nil {
			logger.Log("files", "deleteFile", "requestId", req.requestId, "error", err)
		}

		return deleteFileResponse{
			Err: err,
		}, nil
	}
}

func decodeDeleteFileRequest(_ context.Context, r *http.Request) (interface{}, error) {
	vars := mux.Vars(r)
	id, ok := vars["id"]
	if !ok {
		return nil, ErrBadRouting
	}
	return deleteFileRequest{
		ID:        id,
		requestId: moovhttp.GetRequestId(r),
	}, nil
}

type getFileContentsRequest struct {
	ID string

	requestId string
}

type getFileContentsResponse struct {
	Err error `json:"error"`
}

func (v getFileContentsResponse) error() error { return v.Err }

func getFileContentsEndpoint(s Service, logger log.Logger) endpoint.Endpoint {
	return func(_ context.Context, request interface{}) (interface{}, error) {
		req := request.(getFileContentsRequest)
		r, err := s.GetFileContents(req.ID)

		if req.requestId != "" && logger != nil {
			logger.Log("files", "getFileContents", "requestId", req.requestId, "error", err)
		}
		if err != nil {
			return getFileContentsResponse{Err: err}, nil
		}

		return r, nil
	}
}

func decodeGetFileContentsRequest(_ context.Context, r *http.Request) (interface{}, error) {
	vars := mux.Vars(r)
	id, ok := vars["id"]
	if !ok {
		return nil, ErrBadRouting
	}
	return getFileContentsRequest{
		ID:        id,
		requestId: moovhttp.GetRequestId(r),
	}, nil
}

type validateFileRequest struct {
	ID string

	requestId string
}

type validateFileResponse struct {
	Err error `json:"error"`
}

func (v validateFileResponse) error() error { return v.Err }

func validateFileEndpoint(s Service, logger log.Logger) endpoint.Endpoint {
	return func(_ context.Context, request interface{}) (interface{}, error) {
		req := request.(validateFileRequest)
		err := s.ValidateFile(req.ID)

		if req.requestId != "" && logger != nil {
			logger.Log("files", "validateFile", "requestId", req.requestId, "error", err)
		}

		return validateFileResponse{
			Err: err,
		}, nil
	}
}

func decodeValidateFileRequest(_ context.Context, r *http.Request) (interface{}, error) {
	vars := mux.Vars(r)
	id, ok := vars["id"]
	if !ok {
		return nil, ErrBadRouting
	}
	return validateFileRequest{
		ID:        id,
		requestId: moovhttp.GetRequestId(r),
	}, nil
}
