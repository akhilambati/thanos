// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package e2ebench_test

import (
	"fmt"
	"os"
	execlib "os/exec"
	"path/filepath"
	"testing"

	"github.com/efficientgo/e2e"
	e2edb "github.com/efficientgo/e2e/db"
	e2einteractive "github.com/efficientgo/e2e/interactive"
	"github.com/pkg/errors"
	"github.com/thanos-io/thanos/pkg/objstore/client"
	"github.com/thanos-io/thanos/pkg/objstore/s3"
	"github.com/thanos-io/thanos/pkg/testutil"
	"gopkg.in/yaml.v2"
)

const data = "data"

var (
	maxTimeFresh = `2021-07-27T00:00:00Z`
	maxTimeOld   = `2021-07-20T00:00:00Z`

	store1Data = func() string { a, _ := filepath.Abs(filepath.Join(data, "store1")); return a }()
	store2Data = func() string { a, _ := filepath.Abs(filepath.Join(data, "store2")); return a }()
	prom1Data  = func() string { a, _ := filepath.Abs(filepath.Join(data, "prom1")); return a }()
	prom2Data  = func() string { a, _ := filepath.Abs(filepath.Join(data, "prom2")); return a }()
)

func exec(cmd string, args ...string) error {
	if o, err := execlib.Command(cmd, args...).CombinedOutput(); err != nil {
		return errors.Wrap(err, string(o))
	}
	return nil
}

func createData() (perr error) {
	fmt.Println("Re-creating data (can take minutes)...")
	defer func() {
		if perr != nil {
			_ = os.RemoveAll(data)
		}
	}()

	if err := exec(
		"sh", "-c",
		fmt.Sprintf("mkdir -p %s && "+
			"docker run -i quay.io/thanos/thanosbench:v0.2.0-rc.1 block plan -p continuous-1w-small --labels 'cluster=\"eu-1\"' --labels 'replica=\"0\"' --max-time=%s | "+
			"docker run -v %s/:/shared -i quay.io/thanos/thanosbench:v0.2.0-rc.1 block gen --output.dir /shared", store1Data, maxTimeOld, store1Data),
	); err != nil {
		return err
	}
	if err := exec(
		"sh", "-c",
		fmt.Sprintf("mkdir -p %s && "+
			"docker run -i quay.io/thanos/thanosbench:v0.2.0-rc.1 block plan -p continuous-1w-small --labels 'cluster=\"us-1\"' --labels 'replica=\"0\"' --max-time=%s | "+
			"docker run -v %s/:/shared -i quay.io/thanos/thanosbench:v0.2.0-rc.1 block gen --output.dir /shared", store2Data, maxTimeOld, store2Data),
	); err != nil {
		return err
	}

	if err := exec(
		"sh", "-c",
		fmt.Sprintf("mkdir -p %s && "+
			"docker run -i quay.io/thanos/thanosbench:v0.2.0-rc.1 block plan -p continuous-1w-small --max-time=%s | "+
			"docker run -v %s/:/shared -i quay.io/thanos/thanosbench:v0.2.0-rc.1 block gen --output.dir /shared", prom1Data, maxTimeFresh, prom1Data),
	); err != nil {
		return err
	}
	if err := exec(
		"sh", "-c",
		fmt.Sprintf("mkdir -p %s && "+
			"docker run -i quay.io/thanos/thanosbench:v0.2.0-rc.1 block plan -p continuous-1w-small --max-time=%s | "+
			"docker run -v %s/:/shared -i quay.io/thanos/thanosbench:v0.2.0-rc.1 block gen --output.dir /shared", prom2Data, maxTimeFresh, prom2Data),
	); err != nil {
		return err
	}
	return nil
}

// Test args: -test.timeout 9999m
func TestQueryPushdown_Demo(t *testing.T) {
	// Create 20k series for 2w of TSDB blocks. Cache them to 'data' dir so we don't need to re-create on every run (it takes ~5m).
	_, err := os.Stat(data)
	if os.IsNotExist(err) {
		testutil.Ok(t, createData())
	} else {
		testutil.Ok(t, err)
	}

	e, err := e2e.NewDockerEnvironment("query_pushdown_demo")
	testutil.Ok(t, err)
	t.Cleanup(e.Close)

	// Initialize object storage with two buckets (our long term storage).
	//
	//	┌──────────────┐
	//	│              │
	//	│    Minio     │
	//	│              │
	//	├──────────────┼──────────────────────────────────────────────────┐
	//	│ Bucket: bkt1 │ {cluster=eu1, replica=0} 10k series [t-2w, t-1w] │
	//	├──────────────┼──────────────────────────────────────────────────┘
	//	│              │
	//	├──────────────┼──────────────────────────────────────────────────┐
	//	│ Bucket: bkt2 │ {cluster=us1, replica=0} 10k series [t-2w, t-1w] │
	//	└──────────────┴──────────────────────────────────────────────────┘
	//
	m1 := e2edb.NewMinio(e, "minio-1", "default")
	testutil.Ok(t, exec("cp", "-r", store1Data, filepath.Join(m1.Dir(), "bkt1")))
	testutil.Ok(t, exec("cp", "-r", store2Data, filepath.Join(m1.Dir(), "bkt2")))

	// Create two store gateways, one for each bucket (access point to long term storage).

	//	                    ┌───────────┐
	//	                    │           │
	//	┌──────────────┐    │  Store 1  │
	//	│ Bucket: bkt1 │◄───┤           │
	//	├──────────────┼    └───────────┘
	//	│              │
	//	├──────────────┼    ┌───────────┐
	//	│ Bucket: bkt2 │◄───┤           │
	//	└──────────────┴    │  Store 2  │
	//	                    │           │
	//	                    └───────────┘
	bkt1Config, err := yaml.Marshal(client.BucketConfig{
		Type: client.S3,
		Config: s3.Config{
			Bucket:    "bkt1",
			AccessKey: e2edb.MinioAccessKey,
			SecretKey: e2edb.MinioSecretKey,
			Endpoint:  m1.InternalEndpoint("http"),
			Insecure:  true,
		},
	})
	testutil.Ok(t, err)
	store1 := e2edb.NewThanosStore(e, "store1", bkt1Config, e2edb.WithImage("thanos:latest"))

	bkt2Config, err := yaml.Marshal(client.BucketConfig{
		Type: client.S3,
		Config: s3.Config{
			Bucket:    "bkt2",
			AccessKey: e2edb.MinioAccessKey,
			SecretKey: e2edb.MinioSecretKey,
			Endpoint:  m1.InternalEndpoint("http"),
			Insecure:  true,
		},
	})
	testutil.Ok(t, err)
	store2 := e2edb.NewThanosStore(e, "store2", bkt2Config, e2edb.WithImage("thanos:latest"))

	// Create two Prometheus replicas in HA, and one separate one (short term storage + scraping).
	// Add a Thanos sidecar.
	//
	//                                                         ┌────────────┐
	//    ┌───────────────────────────────────────────────┐    │            │
	//    │ {cluster=eu1, replica=0} 10k series [t-1w, t] │◄───┤  Prom-ha0  │
	//    └───────────────────────────────────────────────┘    │            │
	//                                                         ├────────────┤
	//                                                         │   Sidecar  │
	//                                                         └────────────┘
	//
	//                                                         ┌────────────┐
	//    ┌───────────────────────────────────────────────┐    │            │
	//    │ {cluster=eu1, replica=0} 10k series [t-1w, t] │◄───┤  Prom-ha1  │
	//    └───────────────────────────────────────────────┘    │            │
	//                                                         ├────────────┤
	//                                                         │   Sidecar  │
	//                                                         └────────────┘
	//
	//                                                         ┌────────────┐
	//    ┌───────────────────────────────────────────────┐    │            │
	//    │ {cluster=eu1, replica=0} 10k series [t-1w, t] │◄───┤  Prom 2    │
	//    └───────────────────────────────────────────────┘    │            │
	//                                                         ├────────────┤
	//                                                         │   Sidecar  │
	//                                                         └────────────┘
	promHA0 := e2edb.NewPrometheus(e, "prom-ha0")
	promHA1 := e2edb.NewPrometheus(e, "prom-ha1")
	prom2 := e2edb.NewPrometheus(e, "prom2")

	sidecarHA0 := e2edb.NewThanosSidecar(e, "sidecar-prom-ha0", promHA0, e2edb.WithImage("thanos:latest"))
	sidecarHA1 := e2edb.NewThanosSidecar(e, "sidecar-prom-ha1", promHA1, e2edb.WithImage("thanos:latest"))
	sidecar2 := e2edb.NewThanosSidecar(e, "sidecar2", prom2, e2edb.WithImage("thanos:latest"))

	testutil.Ok(t, exec("cp", "-r", prom1Data, promHA0.Dir()))
	testutil.Ok(t, exec("sh", "-c", "find "+prom1Data+" -maxdepth 1 -type d | tail -5 | xargs cp -r -t "+promHA1.Dir())) // Copy only 5 blocks from 9 to mimic replica 1 with partial data set.
	testutil.Ok(t, exec("cp", "-r", prom2Data, prom2.Dir()))

	testutil.Ok(t, promHA0.SetConfig(`
global:
  external_labels:
    cluster: eu-1
    replica: 0
`,
	))
	testutil.Ok(t, promHA1.SetConfig(`
global:
  external_labels:
    cluster: eu-1
    replica: 1
`,
	))
	testutil.Ok(t, prom2.SetConfig(`
global:
  external_labels:
    cluster: us-1
    replica: 0
`,
	))

	testutil.Ok(t, e2e.StartAndWaitReady(m1))
	testutil.Ok(t, e2e.StartAndWaitReady(promHA0, promHA1, prom2, sidecarHA0, sidecarHA1, sidecar2, store1, store2))

	// Let's start query on top of all those 5 store APIs (global query engine).
	//
	//  ┌───────────┐
	//  │           │
	//  │  Store 1  │◄──────┐
	//  ┤           │       │
	//  └───────────┘       │
	//                      │
	//  ┌───────────┐       │
	//  ┤           │       │
	//  │  Store 2  │◄──────┤
	//  │           │       │
	//  └───────────┘       │
	//                      │
	//                      │
	//                      │      ┌───────────────┐
	//  ┌────────────┐      │      │               │
	//  │            │      ├──────┤    Querier    │◄────── PromQL
	//  ┤  Prom-ha0  │      │      │               │
	//  │            │      │      └───────────────┘
	//  ├────────────┤      │
	//  │   Sidecar  │◄─────┤
	//  └────────────┘      │
	//                      │
	//  ┌────────────┐      │
	//  │            │      │
	//  ┤  Prom-ha1  │      │
	//  │            │      │
	//  ├────────────┤      │
	//  │   Sidecar  │◄─────┤
	//  └────────────┘      │
	//                      │
	//  ┌────────────┐      │
	//  │            │      │
	//  ┤  Prom 2    │      │
	//  │            │      │
	//  ├────────────┤      │
	//  │   Sidecar  │◄─────┘
	//  └────────────┘
	//
	query1 := e2edb.NewThanosQuerier(e, "query1", []string{
		store1.InternalEndpoint("grpc"),
		store2.InternalEndpoint("grpc"),
		sidecarHA0.InternalEndpoint("grpc"),
		sidecarHA1.InternalEndpoint("grpc"),
		sidecar2.InternalEndpoint("grpc"),
	})
	testutil.Ok(t, e2e.StartAndWaitReady(query1))

	// Let's start the party!
	// Wait until we have 5 gRPC connections.
	testutil.Ok(t, query1.WaitSumMetricsWithOptions(e2e.Equals(5), []string{"thanos_store_nodes_grpc_connections"}, e2e.WaitMissingMetrics()))

	testutil.Ok(t, e2einteractive.OpenInBrowser(fmt.Sprintf("http://%s/%s", query1.Endpoint("http"), "graph?g0.expr=count(%7B__name__%3D~\"continuous_app_metric99\"%7D)%20by%20(replica)&g0.tab=0&g0.stacked=0&g0.range_input=2w&g0.max_source_resolution=0s&g0.deduplicate=0&g0.partial_response=0&g0.store_matches=%5B%5D&g0.end_input=2021-07-27%2000%3A00%3A00")))
	testutil.Ok(t, e2einteractive.RunUntilEndpointHit())
}