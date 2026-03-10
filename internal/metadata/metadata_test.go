package metadata

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
)

type fakeIMDSClient struct {
	output *imds.GetInstanceIdentityDocumentOutput
	err    error
}

func (f fakeIMDSClient) GetInstanceIdentityDocument(context.Context, *imds.GetInstanceIdentityDocumentInput, ...func(*imds.Options)) (*imds.GetInstanceIdentityDocumentOutput, error) {
	return f.output, f.err
}

func TestLoadReturnsECSMetadata(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = writer.Write([]byte(`{"Name":"cwproxy","ContainerARN":"arn:aws:ecs:container/123"}`))
		case "/task":
			_, _ = writer.Write([]byte(`{"Cluster":"demo-cluster","TaskARN":"arn:aws:ecs:task/abc","Family":"cwproxy","Revision":"12","LaunchType":"FARGATE","AvailabilityZone":"ap-northeast-2a"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	snapshot := Load(context.Background(), Options{
		LookupEnv: func(key string) (string, bool) {
			if key == "ECS_CONTAINER_METADATA_URI_V4" {
				return server.URL, true
			}
			return "", false
		},
		ReadFile: func(string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		HTTPClient: server.Client(),
		IMDSClient: fakeIMDSClient{err: errors.New("not on ec2")},
	})
	if snapshot == nil || snapshot.ECS == nil {
		t.Fatalf("snapshot = %#v", snapshot)
	}

	if snapshot.ECS.Cluster != "demo-cluster" {
		t.Fatalf("Cluster = %q", snapshot.ECS.Cluster)
	}
	if snapshot.ECS.TaskARN != "arn:aws:ecs:task/abc" {
		t.Fatalf("TaskARN = %q", snapshot.ECS.TaskARN)
	}
	if snapshot.ECS.ContainerName != "cwproxy" {
		t.Fatalf("ContainerName = %q", snapshot.ECS.ContainerName)
	}
	if snapshot.ECS.LaunchType != "FARGATE" {
		t.Fatalf("LaunchType = %q", snapshot.ECS.LaunchType)
	}
}

func TestLoadReturnsEKSMetadata(t *testing.T) {
	t.Parallel()

	token := encodeToken(t, map[string]any{
		"kubernetes.io": map[string]any{
			"namespace": "default",
			"pod": map[string]any{
				"name": "orders-api-75c8b7d7d6-pxk9m",
				"uid":  "pod-uid",
			},
			"serviceaccount": map[string]any{
				"name": "cwproxy-service-account",
				"uid":  "sa-uid",
			},
		},
	})

	snapshot := Load(context.Background(), Options{
		LookupEnv: func(key string) (string, bool) {
			switch key {
			case "KUBERNETES_SERVICE_HOST":
				return "10.0.0.1", true
			case "EKS_CLUSTER_NAME":
				return "demo-eks", true
			case "NODE_NAME":
				return "ip-10-0-0-12", true
			case "AWS_EXECUTION_ENV":
				return "AWS_EKS_FARGATE", true
			default:
				return "", false
			}
		},
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case namespaceFilePath:
				return []byte("default"), nil
			case serviceAccountToken:
				return []byte(token), nil
			default:
				return nil, os.ErrNotExist
			}
		},
		IMDSClient: fakeIMDSClient{err: errors.New("not on ec2")},
	})
	if snapshot == nil || snapshot.EKS == nil {
		t.Fatalf("snapshot = %#v", snapshot)
	}

	if snapshot.EKS.ClusterName != "demo-eks" {
		t.Fatalf("ClusterName = %q", snapshot.EKS.ClusterName)
	}
	if snapshot.EKS.DeploymentName != "orders-api" {
		t.Fatalf("DeploymentName = %q", snapshot.EKS.DeploymentName)
	}
	if snapshot.EKS.Namespace != "default" {
		t.Fatalf("Namespace = %q", snapshot.EKS.Namespace)
	}
	if snapshot.EKS.PodName != "orders-api-75c8b7d7d6-pxk9m" {
		t.Fatalf("PodName = %q", snapshot.EKS.PodName)
	}
	if snapshot.EKS.ServiceAccount != "cwproxy-service-account" {
		t.Fatalf("ServiceAccount = %q", snapshot.EKS.ServiceAccount)
	}
}

func TestLoadReturnsEC2Metadata(t *testing.T) {
	t.Parallel()

	snapshot := Load(context.Background(), Options{
		LookupEnv: func(string) (string, bool) {
			return "", false
		},
		ReadFile: func(string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		IMDSClient: fakeIMDSClient{
			output: &imds.GetInstanceIdentityDocumentOutput{
				InstanceIdentityDocument: imds.InstanceIdentityDocument{
					InstanceID:       "i-1234567890",
					InstanceType:     "m7g.large",
					Region:           "ap-northeast-2",
					AvailabilityZone: "ap-northeast-2a",
					ImageID:          "ami-123456",
					AccountID:        "123456789012",
				},
			},
		},
	})
	if snapshot == nil || snapshot.EC2 == nil {
		t.Fatalf("snapshot = %#v", snapshot)
	}

	if snapshot.EC2.InstanceID != "i-1234567890" {
		t.Fatalf("InstanceID = %q", snapshot.EC2.InstanceID)
	}
	if snapshot.EC2.Region != "ap-northeast-2" {
		t.Fatalf("Region = %q", snapshot.EC2.Region)
	}
	if snapshot.EC2.AccountID != "123456789012" {
		t.Fatalf("AccountID = %q", snapshot.EC2.AccountID)
	}
}

func TestLoadReturnsNilWhenNoMetadataIsAvailable(t *testing.T) {
	t.Parallel()

	snapshot := Load(context.Background(), Options{
		LookupEnv: func(string) (string, bool) {
			return "", false
		},
		ReadFile: func(string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		IMDSClient: fakeIMDSClient{err: errors.New("not on ec2")},
	})
	if snapshot != nil {
		t.Fatalf("snapshot = %#v, want nil", snapshot)
	}
}

func TestInferDefaultAppName(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		snapshot *Snapshot
		want     string
	}{
		{
			name: "eks deployment wins",
			snapshot: &Snapshot{
				EKS: &EKS{DeploymentName: "orders-api"},
				ECS: &ECS{TaskFamily: "orders-task"},
			},
			want: "orders-api",
		},
		{
			name: "ecs task family fallback",
			snapshot: &Snapshot{
				ECS: &ECS{TaskFamily: "orders-task"},
			},
			want: "orders-task",
		},
		{
			name:     "empty snapshot",
			snapshot: &Snapshot{},
			want:     "",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := InferDefaultAppName(testCase.snapshot); got != testCase.want {
				t.Fatalf("InferDefaultAppName() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestInferDeploymentName(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		podName string
		want    string
	}{
		{
			name:    "deployment pod name",
			podName: "orders-api-75c8b7d7d6-pxk9m",
			want:    "orders-api",
		},
		{
			name:    "statefulset pod name does not match",
			podName: "orders-api-0",
			want:    "",
		},
		{
			name:    "invalid replica set hash does not match",
			podName: "orders-api-notahash-pxk9m",
			want:    "",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := inferDeploymentName(testCase.podName); got != testCase.want {
				t.Fatalf("inferDeploymentName() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestInferAWSRegion(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		snapshot *Snapshot
		want     string
	}{
		{
			name: "ec2 region wins",
			snapshot: &Snapshot{
				EC2: &EC2{Region: "ap-northeast-2"},
				ECS: &ECS{
					TaskARN:          "arn:aws:ecs:us-east-1:123456789012:task/abc",
					AvailabilityZone: "us-east-1a",
				},
			},
			want: "ap-northeast-2",
		},
		{
			name: "ecs task arn fallback",
			snapshot: &Snapshot{
				ECS: &ECS{
					TaskARN: "arn:aws:ecs:us-west-2:123456789012:task/abc",
				},
			},
			want: "us-west-2",
		},
		{
			name: "ecs availability zone fallback",
			snapshot: &Snapshot{
				ECS: &ECS{
					AvailabilityZone: "eu-central-1b",
				},
			},
			want: "eu-central-1",
		},
		{
			name:     "missing region",
			snapshot: &Snapshot{},
			want:     "",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := InferAWSRegion(testCase.snapshot); got != testCase.want {
				t.Fatalf("InferAWSRegion() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func encodeToken(t *testing.T, claims map[string]any) string {
	t.Helper()

	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	return strings.Join([]string{
		"header",
		base64.RawURLEncoding.EncodeToString(body),
		"signature",
	}, ".")
}
