/*
 * Copyright 2020 National Library of Norway.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *       http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package index

import (
	"fmt"

	"github.com/nlnwa/gowarc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type CdxWriter interface {
	Init(config *DbConfig) error
	Close()
	Write(wr gowarc.WarcRecord, fileName string, offset int64) error
}

type CdxLegacy struct {
}
type CdxJ struct {
}
type CdxPb struct {
}
type CdxDb struct {
	db *DB
}

func (c *CdxDb) Init(config *DbConfig) (err error) {
	c.db, err = NewIndexDb(config)
	if err != nil {
		return err
	}
	return nil
}

func (c *CdxDb) Close() {
	c.db.Flush()
	c.db.Close()
}

func (c *CdxDb) Write(wr gowarc.WarcRecord, fileName string, offset int64) error {
	return c.db.Add(wr, fileName, offset)
}

func (c *CdxLegacy) Init(config *DbConfig) (err error) {
	return nil
}

func (c *CdxLegacy) Close() {
}

func (c *CdxLegacy) Write(wr gowarc.WarcRecord, fileName string, offset int64) error {
	return nil
}

func (c *CdxJ) Init(config *DbConfig) (err error) {
	return nil
}

func (c *CdxJ) Close() {
}

func (c *CdxJ) Write(wr gowarc.WarcRecord, fileName string, offset int64) error {
	if wr.Type() == gowarc.Response {
		rec := NewCdxRecord(wr, fileName, offset)
		cdxj := protojson.Format(rec)
		fmt.Printf("%s %s %s %s\n", rec.Ssu, rec.Sts, rec.Srt, cdxj)
	}
	return nil
}

func (c *CdxPb) Init(config *DbConfig) (err error) {
	return nil
}

func (c *CdxPb) Close() {
}

func (c *CdxPb) Write(wr gowarc.WarcRecord, fileName string, offset int64) error {
	if wr.Type() == gowarc.Response {
		rec := NewCdxRecord(wr, fileName, offset)
		cdxpb, err := proto.Marshal(rec)
		if err != nil {
			return err
		}
		fmt.Printf("%s %s %s %s\n", rec.Ssu, rec.Sts, rec.Srt, cdxpb)
	}
	return nil
}
