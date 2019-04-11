package serviceprofiles

import (
	"fmt"
	"os"
	"testing"
	"time"

	sp "github.com/linkerd/linkerd2/controller/gen/apis/serviceprofile/v1alpha1"
	"github.com/linkerd/linkerd2/testutil"
	"sigs.k8s.io/yaml"
)

var TestHelper *testutil.TestHelper

type rowStat struct {
	Route            string  `json:"route"`
	Authority        string  `json:"authority"`
	Success          float64 `json:"success"`
	EffectiveSuccess float64 `json:"effective_success"`
	ActualSuccess    float64 `json:"actual_success"`
	RPS              float64 `json:"rps"`
	LatencyP50       int     `json:"latency_ms_p50"`
	LatencyP95       int     `json:"latency_ms_p95"`
	LatencyP99       int     `json:"latency_ms_p99"`
}

type testCase struct {
	args           []string
	deployName     string
	expectedRoutes []string
	namespace      string
	sourceName     string
	spName         string
}

type routeStatAssertion struct {
	upstream      string
	downstream    string
	namespace     string
	routeProperty string
	expected      string
	assertFunc    func(stat *rowStat) bool
}

func TestMain(m *testing.M) {
	TestHelper = testutil.NewTestHelper()
	os.Exit(m.Run())
}

func TestServiceProfiles(t *testing.T) {

	testNamespace := TestHelper.GetTestNamespace("serviceprofile-test")
	out, stderr, err := TestHelper.LinkerdRun("inject", "testdata/tap_application.yaml")
	if err != nil {
		t.Fatalf("'linkerd %s' command failed with %s: %s\n", "inject", err.Error(), stderr)
	}

	out, err = TestHelper.KubectlApply(out, testNamespace)
	if err != nil {
		t.Fatalf("kubectl apply command failed\n%s", out)
	}

	// wait for deployments to start
	for _, deploy := range []string{"t1", "t2", "t3", "gateway"} {
		if err := TestHelper.CheckPods(testNamespace, deploy, 1); err != nil {
			t.Error(err)
		}

		if err := TestHelper.CheckDeployment(testNamespace, deploy, 1); err != nil {
			t.Error(fmt.Errorf("Error validating deployment [%s]:\n%s", deploy, err))
		}
	}

	testCases := []testCase{
		{
			sourceName:     "tap",
			namespace:      testNamespace,
			deployName:     "deployment/t1",
			spName:         "t1-svc",
			expectedRoutes: []string{"POST /buoyantio.bb.TheService/theFunction", "[DEFAULT]"},
		},
		{
			sourceName:     "open-api",
			namespace:      testNamespace,
			spName:         "t3-svc",
			deployName:     "deployment/t3",
			expectedRoutes: []string{"DELETE /testpath", "GET /testpath", "PATCH /testpath", "POST /testpath", "[DEFAULT]"},
		},
	}

	for _, tc := range testCases {
		tc := tc // pin
		t.Run(tc.sourceName, func(t *testing.T) {
			routes, err := getRoutes(tc.deployName, tc.namespace, false, []string{})
			if err != nil {
				t.Fatalf("routes command failed: %s\n", err)
			}

			initialExpectedRoutes := []string{"[DEFAULT]"}

			assertExpectedRoutes(initialExpectedRoutes, routes, t)

			sourceFlag := fmt.Sprintf("--%s", tc.sourceName)
			cmd := []string{"profile", "--namespace", tc.namespace, tc.spName, sourceFlag}
			if tc.sourceName == "tap" {
				tc.args = []string{
					tc.deployName,
					"--tap-route-limit",
					"1",
					"--tap-duration",
					"25s",
				}
			}

			if tc.sourceName == "open-api" {
				tc.args = []string{
					"testdata/t3.swagger",
				}
			}

			cmd = append(cmd, tc.args...)
			out, stderr, err := TestHelper.LinkerdRun(cmd...)
			if err != nil {
				t.Fatalf("'linkerd %s' command failed with %s: %s\n", cmd, err.Error(), stderr)
			}

			_, err = TestHelper.KubectlApply(out, tc.namespace)
			if err != nil {
				t.Fatalf("kubectl apply command failed:\n%s", err)
			}

			routes, err = getRoutes(tc.deployName, tc.namespace, false, []string{})
			if err != nil {
				t.Fatalf("routes command failed: %s\n", err)
			}

			assertExpectedRoutes(tc.expectedRoutes, routes, t)
		})
	}
}

func TestServiceProfileMetrics(t *testing.T) {

	testNamespace := TestHelper.GetTestNamespace("serviceprofile-test")
	testCases := []struct {
		injectYAML string
		test       string
	}{
		{
			"hello_world_retries.yaml",
			"retries",
		},
		{
			"hello_world_timeouts.yaml",
			"timeouts",
		},
	}

	for _, tc := range testCases {
		var (
			tc                   = tc
			testSP               = fmt.Sprintf("world-%s-svc", tc.test)
			testDownstreamDeploy = fmt.Sprintf("deployment/world-%s", tc.test)
			testUpstreamDeploy   = fmt.Sprintf("deployment/hello-%s", tc.test)
		)

		t.Run(tc.test, func(t *testing.T) {
			out, stderr, err := TestHelper.LinkerdRun("inject", fmt.Sprintf("testdata/%s", tc.injectYAML))
			if err != nil {
				t.Errorf("'linkerd %s' command failed with %s: %s\n", "inject", err.Error(), stderr)
			}

			out, err = TestHelper.KubectlApply(out, testNamespace)
			if err != nil {
				t.Errorf("kubectl apply command failed\n%s", out)
			}

			cmd := []string{
				"profile",
				"--namespace",
				testNamespace,
				testSP,
				"--tap",
				testDownstreamDeploy,
				"--tap-duration",
				"25s",
			}

			out, stderr, err = TestHelper.LinkerdRun(cmd...)
			if err != nil {
				t.Errorf("'linkerd %s' command failed with %s: %s\n", cmd, err.Error(), stderr)
			}

			_, err = TestHelper.KubectlApply(out, testNamespace)
			if err != nil {
				t.Errorf("kubectl apply command failed:\n%s", err)
			}

			assertion := &routeStatAssertion{
				upstream:   testUpstreamDeploy,
				downstream: testDownstreamDeploy,
				namespace:  testNamespace,
			}
			switch tc.test {
			case "retries":
				// If the effective success rate is >= we aren't seeing any failures so we can't test retries.
				assertion.assertFunc = func(rt *rowStat) bool { return rt.EffectiveSuccess >= 1 }
				assertion.routeProperty = "Effective Success"
				assertion.expected = "< 1"
				assertRouteStat(assertion, t)
			case "timeouts":
				// If the P99 latency is greater than 250ms retries are probably happening before applying
				// the service profile so we fail the test.
				assertion.assertFunc = func(rt *rowStat) bool { return rt.LatencyP99 >= 250 }
				assertion.routeProperty = "P99 Latency"
				assertion.expected = "< 250"
				assertRouteStat(assertion, t)
			}

			profile := &sp.ServiceProfile{}

			// Grab the output and convert it to a service profile object for modification
			err = yaml.Unmarshal([]byte(out), profile)
			if err != nil {
				t.Errorf("unable to unmarshall YAML: %s", err.Error())
			}

			for _, route := range profile.Spec.Routes {
				if route.Name == "GET /testpath" {
					route.IsRetryable = true
					route.Timeout = "500ms"
					break
				}
			}

			bytes, err := yaml.Marshal(profile)
			if err != nil {
				t.Errorf("error marshalling service profile: %s", bytes)
			}

			_, err = TestHelper.KubectlApply(string(bytes), testNamespace)
			if err != nil {
				t.Errorf("kubectl apply command failed:\n%s", err)
			}

			switch tc.test {
			case "retries":
				// If we get an effective success rate of less than 100% retries aren't happening.
				// after we applied our modified service profile.
				assertion.assertFunc = func(rt *rowStat) bool { return rt.EffectiveSuccess < 1 }
				assertion.routeProperty = "Effective Success"
				assertion.expected = ">= 1"
				assertRouteStat(assertion, t)
				// If we get a P99 latency of less than 250ms then we aren't hitting the timeout limit
				// after setting up the timeout in service profile. hello-timeouts-service always fails
				// so we expect all request latencies to be at least half the total timeout set.
			case "timeouts":
				assertion.assertFunc = func(rt *rowStat) bool { return rt.LatencyP99 < 250 }
				assertion.routeProperty = "P99 Latency"
				assertion.expected = ">= 250"
				assertRouteStat(assertion, t)
			}
		})
	}
}

func assertRouteStat(assertion *routeStatAssertion, t *testing.T) {
	err := TestHelper.RetryFor(10*time.Second, func() error {
		routes, err := getRoutes(assertion.upstream, assertion.namespace, true, []string{"--to", assertion.downstream})
		if err != nil {
			return fmt.Errorf("routes command failed: %s", err)
		}
		assertExpectedRoutes([]string{"GET /testpath", "[DEFAULT]"}, routes, t)

		for _, route := range routes {
			if route.Route == "GET /testpath" && assertion.assertFunc(route) {
				return fmt.Errorf("expected route property [%s] to be [%s]. in [%+v]",
					assertion.routeProperty, assertion.expected, route)
			}
		}
		return nil
	})

	if err != nil {
		t.Error(err.Error())
	}
}

func assertExpectedRoutes(expected []string, actual []*rowStat, t *testing.T) {

	if len(expected) != len(actual) {
		t.Errorf("mismatch routes count. Expected %d, Actual %d", len(expected), len(actual))
	}

	for _, expectedRoute := range expected {
		containsRoute := false
		for _, actualRoute := range actual {
			if actualRoute.Route == expectedRoute {
				containsRoute = true
				break
			}
		}
		if !containsRoute {
			t.Errorf("Expected route %s not found in %v", expectedRoute, actual)
		}
	}
}

func getRoutes(deployName, namespace string, isWideOutput bool, additionalArgs []string) ([]*rowStat, error) {
	cmd := []string{"routes", "--namespace", namespace, deployName}

	if len(additionalArgs) > 0 {
		cmd = append(cmd, additionalArgs...)
	}

	if isWideOutput {
		cmd = append(cmd, "-owide")
	}

	cmd = append(cmd, "--output", "json")
	out, stderr, err := TestHelper.LinkerdRun(cmd...)
	if err != nil {
		return nil, err
	}
	var list map[string][]*rowStat
	err = yaml.Unmarshal([]byte(out), &list)
	if err != nil {
		return nil, fmt.Errorf(fmt.Sprintf("Error: %s stderr: %s", err.Error(), stderr))
	}
	return list[deployName], nil
}