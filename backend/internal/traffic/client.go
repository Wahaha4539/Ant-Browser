package traffic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// No environment proxy: credentials and control calls must stay on loopback.
var controlClient = &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}}

func Call(info Runtime, route string, input, output any) error {
	method := http.MethodGet
	var data []byte
	var err error
	if input != nil {
		method = http.MethodPost
		data, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequest(method, "http://"+info.Address+route, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+info.Token)
	req.Header.Set("Content-Type", "application/json")
	response, err := controlClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("traffic service %d: %s", response.StatusCode, string(b))
	}
	if output != nil {
		return json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(output)
	}
	_, err = io.Copy(io.Discard, response.Body)
	return err
}
