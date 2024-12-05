package transport

import (
	"encoding/json"
	"fmt"
	"github.com/ginharu/ethgo/jsonrpc/codec"
	"github.com/valyala/fasthttp"
	"time"
)

// HTTP is an http transport
type HTTP struct {
	addr    string
	client  *fasthttp.Client
	headers map[string]string
}

func newHTTP(addr string, headers map[string]string) *HTTP {
	return &HTTP{
		addr: addr,
		client: &fasthttp.Client{
			DialDualStack:       true,
			MaxConnsPerHost:     1000,
			MaxIdleConnDuration: 5 * time.Second, //// 空闲链接时间应短，避免请求服务的 keep-alive 过短主动关闭
			MaxConnDuration:     10 * time.Minute,
			ReadTimeout:         30 * time.Second,
			WriteTimeout:        30 * time.Second,
			MaxResponseBodySize: 1024 * 1024 * 10,
			MaxConnWaitTimeout:  time.Minute,
			//Dial: func(addr string) (net.Conn, error) {
			//	idx := 3 // 重试三次
			//	for {
			//		idx--
			//		conn, err := defaultDialer.DialTimeout(addr, 10*time.Second) // tcp连接超时时间10s
			//		if err != fasthttp.ErrDialTimeout || idx == 0 {
			//			return conn, err
			//		}
			//	}
			//},
		},

		headers: headers,
	}
}

// Close implements the transport interface
func (h *HTTP) Close() error {
	return nil
}

// Call implements the transport interface
func (h *HTTP) Call(method string, out interface{}, params ...interface{}) error {
	// Encode json-rpc request
	request := codec.Request{
		JsonRPC: "2.0",
		Method:  method,
	}
	if len(params) > 0 {
		data, err := json.Marshal(params)
		if err != nil {
			return err
		}
		request.Params = data
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI(h.addr)
	req.Header.SetMethod("POST")
	req.Header.SetContentType("application/json")
	for k, v := range h.headers {
		req.Header.Add(k, v)
	}
	req.SetBody(raw)

	if err := h.client.Do(req, res); err != nil {
		return err
	}

	if sc := res.StatusCode(); sc != fasthttp.StatusOK {
		return fmt.Errorf("status code is %d. response = %s", sc, string(res.Body()))
	}

	// Decode json-rpc response
	var response codec.Response
	if err := json.Unmarshal(res.Body(), &response); err != nil {
		return err
	}
	if response.Error != nil {
		return response.Error
	}


	if err := json.Unmarshal(response.Result, out); err != nil {
		return err
	}
	return nil
}

// SetMaxConnsPerHost sets the maximum number of connections that can be established with a host
func (h *HTTP) SetMaxConnsPerHost(count int) {
	h.client.MaxConnsPerHost = count
}
