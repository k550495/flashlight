package client

import (
	"net/http"
	"testing"
)

func TestRandomServer(t *testing.T) {
	client := &Client{
		servers: []*server{
			&server{
				weight: 500,
				qos:    0,
			},
		},
		totalServerWeights: 500,
	}

	req, _ := http.NewRequest("POST", "http://localhost/index.html", nil)

	srv := client.randomServer(req)
	if srv == nil {
		t.Errorf("randomServer with just 1 server should have returned that 1 server")
	}

	req.Header.Set(X_FLASHLIGHT_QOS, "5")
	srv = client.randomServer(req)
	if srv == nil {
		t.Errorf("randomServer with just 1 server and overly high QOS should have returned 1 and only server")
	}

	client.servers = append(client.servers, &server{
		weight: 1000,
		qos:    5,
	}, &server{
		weight: 1500,
		qos:    10,
	})
	client.totalServerWeights = 3000

	req.Header.Del(X_FLASHLIGHT_QOS)

	freqs := map[int]float32{
		500:  0,
		1000: 0,
		1500: 0,
	}

	// Do a bunch of random trials
	for i := 0; i < 3000; i++ {
		srv := client.randomServer(req)
		freqs[srv.weight] = freqs[srv.weight] + 1
	}

	for weight, freq := range freqs {
		if freq < float32(weight)-100 || freq > float32(weight)+100 {
			t.Errorf("At QOS 0, weight %d was found an incorrect number of times: %f", weight, freq)
		}
	}

	freqs = map[int]float32{
		500:  0,
		1000: 0,
		1500: 0,
	}

	req.Header.Set(X_FLASHLIGHT_QOS, "5")
	// Do a bunch of random trials
	for i := 0; i < 2500; i++ {
		srv := client.randomServer(req)
		freqs[srv.weight] = freqs[srv.weight] + 1
	}

	for weight, freq := range freqs {
		if weight == 500 {
			if freq > 0 {
				t.Errorf("At QOS 5, weight 500 should not have ever been found")
			}
		} else {
			if freq < float32(weight)-200 || freq > float32(weight)+200 {
				t.Errorf("At QOS 5, weight %d was found an incorrect number of times: %f", weight, freq)
			}
		}
	}
}
