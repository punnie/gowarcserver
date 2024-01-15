package keyvalue

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nlnwa/gowarcserver/index"
	"github.com/nlnwa/gowarcserver/schema"
	"github.com/nlnwa/gowarcserver/server/api"
	"github.com/rs/zerolog/log"
	"golang.org/x/net/publicsuffix"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type ReportGenerator struct {
	Id              string
	UpdateInterval  time.Duration
	UpdateThreshold int
	index.ReportGenerator
}

func NewReportGenerator(g index.ReportGenerator) (*ReportGenerator, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	return &ReportGenerator{
		Id:              id.String(),
		UpdateInterval:  5 * time.Second,
		UpdateThreshold: 100000,
		ReportGenerator: g,
	}, nil
}

// func decode(any interface{}) (*structpb.Struct, error) {
// 	var dataMap map[string]interface{}
// 	err := mapstructure.Decode(any, &dataMap)
// 	if err != nil {
// 		return nil, err
// 	}
// 	return structpb.NewStruct(dataMap)
// }

func mapRequestToStructPb(req index.Request) (*structpb.Struct, error) {
	values := req.(*api.SearchRequest).Values
	m := make(map[string]interface{}, len(values))
	for k, v := range values {
		if len(v) == 1 {
			m[k] = v[0]
		} else {
			m[k] = strings.Join(v, ",")
		}
	}
	return structpb.NewStruct(m)
}

func (r ReportGenerator) Generate(ctx context.Context, req index.Request) (*schema.Report, error) {
	if r.Id == "" {
		return nil, fmt.Errorf("report generator id is empty")
	}

	query, err := mapRequestToStructPb(req)
	if err != nil {
		return nil, err
	}

	report := &schema.Report{
		Id:        r.Id,
		StartTime: timestamppb.New(time.Now()),
		Status:    schema.Report_PENDING,
		Query:     query,
	}
	err = r.SaveReport(ctx, report)
	if err != nil {
		return nil, err
	}

	go func() {
		var err error

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r.AddTask(r.Id, cancel)
		defer r.DeleteTask(r.Id)

		report.Status = schema.Report_RUNNING
		reportData := &schema.ReportData{
			CountByStatusCode:  make(map[string]uint64),
			CountByRecordType:  make(map[string]uint64),
			CountByContentType: make(map[string]uint64),
			CountByScheme:      make(map[string]uint64),
		}
		report.Data = reportData

		defer func() {
			if err != nil {
				report.Error = err.Error()
				report.Status = schema.Report_FAILED
			} else {
				report.Progress = ""
				report.Status = schema.Report_COMPLETED
			}
			report.EndTime = timestamppb.New(time.Now())
			report.Duration = durationpb.New(report.EndTime.AsTime().Sub(report.StartTime.AsTime()))

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = r.SaveReport(ctx, report)
			if err != nil {
				log.Error().Err(err).Msg("failed to save report")
			}
		}()

		results := make(chan index.CdxResponse)
		err = r.Search(ctx, req, results)
		if err != nil {
			return
		}

		var (
			resp                       CdxResponse
			key                        CdxKey
			cdx                        *schema.Cdx
			target, prevTarget         string
			surtDomain, prevSurtDomain string
			ts, prevTs                 time.Time
			path, prevPath             string
		)

		updateCount := 0
		tick := time.NewTicker(r.UpdateInterval)
		defer tick.Stop()
		for result := range results {
			tock := false
			select {
			case <-ctx.Done():
				err = ctx.Err()
				return
			case <-tick.C:
				tock = true
			default:
			}
			resp = result.(CdxResponse)
			key = make([]byte, len(resp.Key))
			copy(key, resp.Key)
			log.Debug().Str("key", string(key)).Msg("")
			cdx = resp.Value

			if tock || updateCount%r.UpdateThreshold == 0 {
				report.Duration = durationpb.New(time.Since(report.StartTime.AsTime()))
				report.Progress = string(key)
				updateCount = 0

				err = r.SaveReport(ctx, report)
				if err != nil {
					return
				}
			}

			reportData.NrOfRecords++
			updateCount++

			err = result.GetError()
			if err != nil {
				return
			}

			// Update surtDomain
			prevSurtDomain = surtDomain
			surtDomain = key.Domain()

			if surtDomain != prevSurtDomain {
				// Increment number of domains
				reportData.NrOfDomains++

				// Update target
				prevTarget = target
				target, err = publicsuffix.EffectiveTLDPlusOne(deSurtDomain(surtDomain))
				if err != nil {
					err = fmt.Errorf("failed to get effective tld plus one for '%s': %w", surtDomain, err)
					return
				}
				if prevTarget != target {
					// Increment number of targets
					reportData.NrOfTargets++
				}
			}

			prevPath = path
			path = key.Path()

			prevTs = ts
			ts = key.Time()

			// A target capture is a capture of a url with path "/"
			if path == "/" {
				// Same domain and path within 5 seconds
				// is not counted as a new target capture
				// to avoid counting immediate redirects.
				if prevPath == path &&
					surtDomain == prevSurtDomain &&
					ts.Sub(prevTs) > 5*time.Second {
					reportData.NrOfTargetCaptures++
				} else {
					reportData.NrOfTargetCaptures++
				}
			}

			// Different path or domain means new url
			// (deliberatly ignoring scheme, port and userinfo).
			if path != prevPath || surtDomain != prevSurtDomain {
				reportData.NrOfUrls++
			}

			reportData.CountByStatusCode[strconv.Itoa(int(cdx.Hsc))]++
			reportData.CountByRecordType[cdx.Srt]++
			reportData.CountByContentType[cdx.Mct]++
			reportData.CountByScheme[key.Scheme()]++
			reportData.ContentLength += uint64(cdx.Cle)
			reportData.PayloadLength += uint64(cdx.Ple)
			reportData.RecordLength += uint64(cdx.Rle)
		}
	}()

	return report, nil
}
