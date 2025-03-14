package transport

import (
	"encoding/json"
	"fmt"
	"github.com/ginharu/ethgo/jsonrpc/codec"
	"github.com/valyala/fasthttp"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// HTTP is a http transport
type HTTP struct {
	addr    string
	client  *fasthttp.Client
	headers map[string]string
	stats   struct {
		totalRequests     int64
		failedRequests    int64
		connectionResets  int64
		lastResetTime     time.Time
		consecutiveErrors int
	}
	statsMu sync.Mutex
}

func newHTTP(addr string, headers map[string]string) *HTTP {
	h := &HTTP{
		addr: addr,
		client: &fasthttp.Client{
			DialDualStack:            true,
			MaxConnsPerHost:          1000,
			MaxIdleConnDuration:      30 * time.Second,
			MaxConnDuration:          10 * time.Minute,
			ReadTimeout:              30 * time.Second,
			WriteTimeout:             30 * time.Second,
			MaxResponseBodySize:      1024 * 1024 * 1000,
			MaxConnWaitTimeout:       5 * time.Second,
			NoDefaultUserAgentHeader: true,
			Dial: func(addr string) (net.Conn, error) {
				retries := 3
				var lastErr error

				for i := 0; i < retries; i++ {
					conn, err := fasthttp.DialTimeout(addr, 5*time.Second)
					if err == nil {
						if tcpConn, ok := conn.(*net.TCPConn); ok {
							tcpConn.SetKeepAlive(true)
							tcpConn.SetKeepAlivePeriod(15 * time.Second)
						}
						return conn, nil
					}

					lastErr = err
					if i < retries-1 {
						time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
					}
				}

				return nil, fmt.Errorf("连接失败，重试 %d 次后: %v", retries, lastErr)
			},
		},
		headers: headers,
	}

	// 启动后台健康检查
	// go h.startHealthChecker()

	return h
}

// Close implements the transport interface
func (h *HTTP) Close() error {
	return nil
}

// Call implements the transport interface
func (h *HTTP) Call(method string, out interface{}, params ...interface{}) error {
	maxRetries := 2
	var lastErr error

	for i := 0; i <= maxRetries; i++ {
		err := h.doSingleCall(method, out, params...)
		if err == nil {
			return nil
		}

		lastErr = err
		// 只有连接错误才重试
		if !isConnectionError(err) {
			return err
		}

		if i < maxRetries {
			// 指数退避策略
			backoff := time.Duration(1<<uint(i)) * 200 * time.Millisecond
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
			time.Sleep(backoff)
		}
	}

	return fmt.Errorf("请求失败，重试 %d 次后: %v", maxRetries, lastErr)
}

// 执行单次调用
func (h *HTTP) doSingleCall(method string, out interface{}, params ...interface{}) error {
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
	if len(params) <= 0 && method == "eth_blockNumber" {
		request.Params = []byte("[]")
	}

	//data3, err := json.MarshalIndent(request, "", "    ")
	//fmt.Println(fmt.Sprintf("%s, %+v", string(data3), err))

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
	//fmt.Print("headers: ", h.headers, "\n")
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

func (h *HTTP) SetUserAgent(userAgent string) {
	h.headers["Accept"] = "application/json"
	h.headers["User-Agent"] = userAgent
}

// 判断是否是连接相关错误
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := err.Error()
	connectionErrors := []string{
		"connection reset by peer",
		"broken pipe",
		"connection refused",
		"connection closed",
		"EOF",
		"i/o timeout",
		"server closed connection",
		"use of closed network connection",
	}

	for _, msg := range connectionErrors {
		if strings.Contains(strings.ToLower(errMsg), msg) {
			return true
		}
	}

	return false
}

// 后台健康检查
func (h *HTTP) startHealthChecker() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// 执行健康检查
			if err := h.checkPoolHealth(); err != nil {
				// 记录错误并重置连接池
				log.Printf("连接池健康检查失败: %v, 正在重置连接池", err)
				h.resetConnectionPool()
			}
		}
	}
}

// 检查连接池健康状态
func (h *HTTP) checkPoolHealth() error {
	// 发送一个简单的请求来测试连接
	var result string
	err := h.doSingleCall("eth_blockNumber", &result)
	return err
}

// 更新请求统计
func (h *HTTP) updateRequestStats(success bool) {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()

	h.stats.totalRequests++

	if !success {
		h.stats.failedRequests++
		h.stats.consecutiveErrors++

		// 如果连续错误达到阈值，重置连接池
		if h.stats.consecutiveErrors >= 5 {
			h.resetConnectionPoolLocked()
		}
	} else {
		h.stats.consecutiveErrors = 0
	}
}

// 重置连接池
func (h *HTTP) resetConnectionPool() {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()

	h.resetConnectionPoolLocked()
}

// 带锁的连接池重置
func (h *HTTP) resetConnectionPoolLocked() {
	// 创建新的客户端实例
	h.client = &fasthttp.Client{
		DialDualStack:            h.client.DialDualStack,
		MaxConnsPerHost:          h.client.MaxConnsPerHost,
		MaxIdleConnDuration:      h.client.MaxIdleConnDuration,
		MaxConnDuration:          h.client.MaxConnDuration,
		ReadTimeout:              h.client.ReadTimeout,
		WriteTimeout:             h.client.WriteTimeout,
		MaxResponseBodySize:      h.client.MaxResponseBodySize,
		MaxConnWaitTimeout:       h.client.MaxConnWaitTimeout,
		NoDefaultUserAgentHeader: h.client.NoDefaultUserAgentHeader,
		Dial:                     h.client.Dial,
	}

	h.stats.connectionResets++
	h.stats.lastResetTime = time.Now()
	h.stats.consecutiveErrors = 0
}

// 获取连接池统计信息
func (h *HTTP) GetPoolStats() interface{} {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()

	return struct {
		TotalRequests    int64     `json:"totalRequests"`
		FailedRequests   int64     `json:"failedRequests"`
		ConnectionResets int64     `json:"connectionResets"`
		LastResetTime    time.Time `json:"lastResetTime"`
		ErrorRate        float64   `json:"errorRate"`
	}{
		TotalRequests:    h.stats.totalRequests,
		FailedRequests:   h.stats.failedRequests,
		ConnectionResets: h.stats.connectionResets,
		LastResetTime:    h.stats.lastResetTime,
		ErrorRate:        float64(h.stats.failedRequests) / float64(max(h.stats.totalRequests, 1)),
	}
}

// 辅助函数
func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
