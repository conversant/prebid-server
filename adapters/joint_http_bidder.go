package adapters

import (
	"bytes"
	"context"
	"github.com/mxmCherry/openrtb"
	"github.com/prebid/prebid-server/openrtb_ext"
	"golang.org/x/net/context/ctxhttp"
	"io/ioutil"
	"net/http"
	"time"
)

// HttpBidder is the interface which almost all bidders implement.
//
// Its only responsibility is to make HTTP request(s) from a BidRequest, and return Bids from the
// HTTP response(s).
type HttpBidder interface {
	// MakeHttpRequests makes the HTTP requests which should be made to fetch bids.
	//
	// The errors should contain a list of errors which explain why this bidder's bids will be
	// "subpar" in some way. For example: the request contained ad types which this bidder doesn't support.
	MakeHttpRequests(request *openrtb.BidRequest) ([]*RequestData, []error)

	// MakeBids unpacks the server's response into Bids.
	// This method **should not** close the response body. The caller will fully read and close it so that the
	// connections get reused properly.
	//
	// The bids can be nil (for no bids), but should not contain nil elements.
	//
	// The errors should contain a list of errors which explain why this bidder's bids will be
	// "subpar" in some way. For example: the server response didn't have the expected format.
	MakeBids(request *openrtb.BidRequest, response *ResponseData) ([]*TypedBid, []error)
}

// AdaptHttpBidder bridges the APIs between a Bidder and an HttpBidder.
func AdaptHttpBidder(bidderCode string, bidder HttpBidder, client *http.Client) Bidder {
	return &bidderAdapter{
		Bidder:     bidder,
		BidderCode: bidderCode,
		Client:     client,
	}
}

type bidderAdapter struct {
	Bidder     HttpBidder
	BidderCode string
	Client     *http.Client
}

func (bidder *bidderAdapter) Bid(ctx context.Context, request *openrtb.BidRequest) (*PBSOrtbSeatBid, []error) {
	start := time.Now()
	reqData, errs := bidder.Bidder.MakeHttpRequests(request)

	if len(reqData) == 0 {
		return nil, errs
	}

	responseChannel := make(chan *httpCallInfo, len(reqData))
	if len(reqData) == 1 {
		responseChannel <- bidder.doRequest(ctx, reqData[0])
	} else {
		for _, oneReqData := range reqData {
			go func() {
				responseChannel <- bidder.doRequest(ctx, oneReqData)
			}()
		}
	}

	seatBid := &PBSOrtbSeatBid{
		Bids:        make([]*PBSOrtbBid, 0, len(reqData)),
		ServerCalls: make([]*openrtb_ext.ExtServerCall, 0, len(reqData)),
	}
	for i := 0; i < len(reqData); i++ {
		httpInfo := <-responseChannel
		seatBid.ServerCalls = append(seatBid.ServerCalls, makeExt(httpInfo))

		if httpInfo.err == nil {
			bids, moreErrs := bidder.Bidder.MakeBids(request, httpInfo.response)
			errs = append(errs, moreErrs...)
			responseTime := int(time.Since(start) / time.Millisecond)
			for i := 0; i < len(bids); i++ {
				seatBid.Bids = append(seatBid.Bids, &PBSOrtbBid{
					Bid:                bids[i].Bid,
					Cache:              nil, // TODO: Cache properly
					Type:               bids[i].BidType,
					ResponseTimeMillis: responseTime,
				})
			}
		} else {
			errs = append(errs, httpInfo.err)
		}
	}

	return seatBid, errs
}

func makeExt(httpInfo *httpCallInfo) *openrtb_ext.ExtServerCall {
	if httpInfo.err == nil {
		return &openrtb_ext.ExtServerCall{
			Uri:          httpInfo.request.Uri,
			RequestBody:  string(httpInfo.request.Body),
			ResponseBody: string(httpInfo.response.Body),
			Status:       httpInfo.response.StatusCode,
		}
	} else {
		return &openrtb_ext.ExtServerCall{
			Uri:         httpInfo.request.Uri,
			RequestBody: string(httpInfo.request.Body),
			Status:      -1,
		}
	}
}

func (bidder *bidderAdapter) doRequest(ctx context.Context, req *RequestData) *httpCallInfo {
	httpReq, err := http.NewRequest("POST", req.Uri, bytes.NewBuffer(req.Body))
	if err != nil {
		return &httpCallInfo{
			request: req,
			err:     err,
		}
	}
	httpReq.Header = req.Headers

	httpResp, err := ctxhttp.Do(ctx, bidder.Client, httpReq)
	if err != nil {
		return &httpCallInfo{
			request: req,
			err:     err,
		}
	}

	respBody, err := ioutil.ReadAll(httpResp.Body)
	if err != nil {
		return &httpCallInfo{
			request: req,
			err:     err,
		}
	}
	defer httpResp.Body.Close()

	return &httpCallInfo{
		request: req,
		response: &ResponseData{
			StatusCode: httpResp.StatusCode,
			Body:       respBody,
			Headers:    httpResp.Header,
		},
		err: err,
	}
}

type httpCallInfo struct {
	request  *RequestData
	response *ResponseData
	err      error
}

// TypedBid packages the openrtb.Bid with any bidder-specific information that PBS needs to populate an
// openrtb_ext.ExtBidPrebid.
//
// PBS will use TypedBid.Bid.Ext to populate "response.seatbid[i].bid.ext.bidder" in the final PBS response,
// and the TypedBid.BidType to populate "response.seatbid[i].bid.ext.prebid.type".
//
// All other fields from the openrtb_ext.ExtBidPrebid can be built uniformly across all HttpBidders...
// so there's no reason that each individual bidder needs to send them.
type TypedBid struct {
	Bid     *openrtb.Bid
	BidType openrtb_ext.BidType
}

// ResponseData packages together information from the server's http.Response.
//
// This exists so that prebid-server core code can implement its "debug" API uniformly across all adapters.
// It will also let us test valyala/vasthttp vs. net/http without changing all the adapters
type ResponseData struct {
	StatusCode int
	Body       []byte
	Headers    http.Header
}

// RequestData packages together the fields needed to make an http.Request.
//
// This exists so that prebid-server core code can implement its "debug" API uniformly across all adapters.
// It will also let us test valyala/vasthttp vs. net/http without changing all the adapters
type RequestData struct {
	Method  string
	Uri     string
	Body    []byte
	Headers http.Header
}
