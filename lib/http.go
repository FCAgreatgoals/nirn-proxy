package lib

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"strings"
)

func copyHeader(dst, src http.Header) {
	dst["Date"] = nil
	dst["Content-Type"] = nil
	for k, vv := range src {
		for _, v := range vv {
			if k != "Content-Length" {
				dst[strings.ToLower(k)] = []string{v}
			}
		}
	}
}

func CopyResponseToResponseWriter(resp *http.Response, respWriter *http.ResponseWriter) error {
	writer := *respWriter
	body, err := ioutil.ReadAll(resp.Body)
	_ = resp.Body.Close()
	// Leave the body readable: the queue still has to look at it once the
	// client has its copy, to tell an unknown webhook from a missing message.
	resp.Body = ioutil.NopCloser(bytes.NewReader(body))
	if err != nil {
		writer.WriteHeader(500)
		_, _ = writer.Write([]byte(err.Error()))
		return err
	}

	copyHeader(writer.Header(), resp.Header)
	writer.WriteHeader(resp.StatusCode)

	_, err = writer.Write(body)
	if err != nil {
		return err
	}
	return nil
}