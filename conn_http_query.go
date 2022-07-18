// Licensed to ClickHouse, Inc. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. ClickHouse, Inc. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package clickhouse

import (
	"bytes"
	"context"
	"errors"
	chproto "github.com/ClickHouse/ch-go/proto"
	"github.com/ClickHouse/clickhouse-go/v2/lib/proto"
	"io"
	"strings"
)

// release is ignored, because http used by std with empty release function
func (h *httpConnect) query(ctx context.Context, release func(*connect, error), query string, args ...interface{}) (*rows, error) {
	query, err := bind(h.location, query, args...)
	if err != nil {
		return nil, err
	}
	options := queryOptions(ctx)
	headers := make(map[string]string)
	switch h.compression {
	case CompressionZSTD, CompressionLZ4:
		options.settings["compress"] = "1"
	case CompressionGZIP, CompressionDeflate:
		// request encoding
		headers["Accept-Encoding"] = h.compression.String()
	}

	res, err := h.sendQuery(ctx, strings.NewReader(query), &options, headers)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	// detect compression from http Content-Encoding header - note user will need to have set enable_http_compression
	// for CH to respond with compressed data - we don't set this automatically as they might not have permissions
	var body []byte
	//adding Accept-Encoding:gzip on your request means response won’t be automatically decompressed per https://github.com/golang/go/blob/master/src/net/http/transport.go#L182-L190

	rw := h.compressionPool.Get()
	body, err = rw.read(res)
	if err != nil {
		return nil, err
	}
	h.compressionPool.Put(rw)
	reader := chproto.NewReader(bytes.NewReader(body))
	block, err := h.readData(reader)
	if err != nil {
		return nil, err
	}

	var (
		errCh  = make(chan error)
		stream = make(chan *proto.Block, 2)
	)

	go func() {
		for {
			block, err := h.readData(reader)
			if err != nil {
				// ch-go wraps EOF errors
				if !errors.Is(err, io.EOF) {
					errCh <- err
				}
				break
			}
			select {
			case <-ctx.Done():
				errCh <- ctx.Err()
				break
			case stream <- block:
			}
		}
		close(stream)
		close(errCh)
	}()

	return &rows{
		block:     block,
		stream:    stream,
		errors:    errCh,
		columns:   block.ColumnsNames(),
		structMap: &structMap{},
	}, nil
}