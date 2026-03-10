package metadata

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
)

const (
	defaultTimeout       = 200 * time.Millisecond
	namespaceFilePath    = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	serviceAccountToken  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	maxMetadataBodyBytes = 64 << 10
)

type Snapshot struct {
	EC2 *EC2 `json:"ec2,omitempty"`
	ECS *ECS `json:"ecs,omitempty"`
	EKS *EKS `json:"eks,omitempty"`
}

type EC2 struct {
	InstanceID       string `json:"instance_id,omitempty"`
	InstanceType     string `json:"instance_type,omitempty"`
	Region           string `json:"region,omitempty"`
	AvailabilityZone string `json:"availability_zone,omitempty"`
	ImageID          string `json:"image_id,omitempty"`
	AccountID        string `json:"account_id,omitempty"`
}

type ECS struct {
	Cluster          string `json:"cluster,omitempty"`
	TaskARN          string `json:"task_arn,omitempty"`
	TaskFamily       string `json:"task_family,omitempty"`
	TaskRevision     string `json:"task_revision,omitempty"`
	LaunchType       string `json:"launch_type,omitempty"`
	AvailabilityZone string `json:"availability_zone,omitempty"`
	ContainerARN     string `json:"container_arn,omitempty"`
	ContainerName    string `json:"container_name,omitempty"`
}

type EKS struct {
	ClusterName          string `json:"cluster_name,omitempty"`
	Namespace            string `json:"namespace,omitempty"`
	PodName              string `json:"pod_name,omitempty"`
	PodUID               string `json:"pod_uid,omitempty"`
	ServiceAccount       string `json:"service_account,omitempty"`
	ServiceAccountUID    string `json:"service_account_uid,omitempty"`
	NodeName             string `json:"node_name,omitempty"`
	ExecutionEnvironment string `json:"execution_environment,omitempty"`
}

type Options struct {
	LookupEnv  func(string) (string, bool)
	ReadFile   func(string) ([]byte, error)
	HTTPClient *http.Client
	IMDSClient imdsClient
	Reporter   func(string, ...any)
}

type imdsClient interface {
	GetInstanceIdentityDocument(context.Context, *imds.GetInstanceIdentityDocumentInput, ...func(*imds.Options)) (*imds.GetInstanceIdentityDocumentOutput, error)
}

type ecsTaskResponse struct {
	Cluster          string `json:"Cluster"`
	TaskARN          string `json:"TaskARN"`
	Family           string `json:"Family"`
	Revision         string `json:"Revision"`
	LaunchType       string `json:"LaunchType"`
	AvailabilityZone string `json:"AvailabilityZone"`
}

type ecsContainerResponse struct {
	ContainerARN string `json:"ContainerARN"`
	Name         string `json:"Name"`
}

type kubernetesTokenClaims struct {
	Kubernetes *struct {
		Namespace string `json:"namespace"`
		Pod       *struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"pod"`
		ServiceAccount *struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"serviceaccount"`
	} `json:"kubernetes.io"`
}

func Load(ctx context.Context, options Options) *Snapshot {
	lookupEnv := options.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	readFile := options.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}

	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}

	imdsClient := options.IMDSClient
	if imdsClient == nil {
		imdsClient = imds.New(imds.Options{
			HTTPClient: httpClient,
		})
	}

	snapshot := &Snapshot{
		ECS: loadECS(ctx, lookupEnv, httpClient, options.Reporter),
		EKS: loadEKS(lookupEnv, readFile),
		EC2: loadEC2(ctx, imdsClient),
	}
	if snapshot.EC2 == nil && snapshot.ECS == nil && snapshot.EKS == nil {
		return nil
	}
	return snapshot
}

func loadEC2(ctx context.Context, client imdsClient) *EC2 {
	if client == nil {
		return nil
	}

	output, err := client.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil || output == nil {
		return nil
	}

	metadata := &EC2{
		InstanceID:       strings.TrimSpace(output.InstanceID),
		InstanceType:     strings.TrimSpace(output.InstanceType),
		Region:           strings.TrimSpace(output.Region),
		AvailabilityZone: strings.TrimSpace(output.AvailabilityZone),
		ImageID:          strings.TrimSpace(output.ImageID),
		AccountID:        strings.TrimSpace(output.AccountID),
	}
	if isZeroEC2(metadata) {
		return nil
	}
	return metadata
}

func loadECS(ctx context.Context, lookupEnv func(string) (string, bool), client *http.Client, reporter func(string, ...any)) *ECS {
	endpoint := firstNonEmptyEnv(lookupEnv, "ECS_CONTAINER_METADATA_URI_V4", "ECS_CONTAINER_METADATA_URI")
	if endpoint == "" || client == nil {
		return nil
	}

	endpoint = strings.TrimRight(endpoint, "/")
	task := ecsTaskResponse{}
	container := ecsContainerResponse{}
	var taskLoaded bool
	var containerLoaded bool

	if err := readJSON(ctx, client, endpoint+"/task", &task); err != nil {
		if reporter != nil {
			reporter("failed to load ECS task metadata: %v", err)
		}
	} else {
		taskLoaded = true
	}

	if err := readJSON(ctx, client, endpoint, &container); err != nil {
		if reporter != nil {
			reporter("failed to load ECS container metadata: %v", err)
		}
	} else {
		containerLoaded = true
	}

	if !taskLoaded && !containerLoaded {
		return nil
	}

	metadata := &ECS{
		Cluster:          strings.TrimSpace(task.Cluster),
		TaskARN:          strings.TrimSpace(task.TaskARN),
		TaskFamily:       strings.TrimSpace(task.Family),
		TaskRevision:     strings.TrimSpace(task.Revision),
		LaunchType:       strings.TrimSpace(task.LaunchType),
		AvailabilityZone: strings.TrimSpace(task.AvailabilityZone),
		ContainerARN:     strings.TrimSpace(container.ContainerARN),
		ContainerName:    strings.TrimSpace(container.Name),
	}
	if isZeroECS(metadata) {
		return nil
	}
	return metadata
}

func loadEKS(lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error)) *EKS {
	executionEnvironment := strings.TrimSpace(envValue(lookupEnv, "AWS_EXECUTION_ENV"))
	clusterName := firstNonEmptyEnv(lookupEnv, "EKS_CLUSTER_NAME", "KUBERNETES_CLUSTER_NAME", "CLUSTER_NAME")
	nodeName := firstNonEmptyEnv(lookupEnv, "NODE_NAME", "K8S_NODE_NAME")
	namespace := firstNonEmptyEnv(lookupEnv, "POD_NAMESPACE", "K8S_NAMESPACE")
	podName := firstNonEmptyEnv(lookupEnv, "POD_NAME")
	serviceAccount := firstNonEmptyEnv(lookupEnv, "SERVICE_ACCOUNT", "K8S_SERVICE_ACCOUNT")

	namespaceBytes, namespaceErr := readFile(namespaceFilePath)
	if namespace == "" && namespaceErr == nil {
		namespace = strings.TrimSpace(string(namespaceBytes))
	}

	tokenClaims := kubernetesTokenClaims{}
	tokenBytes, tokenErr := readFile(serviceAccountToken)
	if err := decodeKubernetesToken(tokenBytes, &tokenClaims); err == nil && tokenClaims.Kubernetes != nil {
		if namespace == "" {
			namespace = strings.TrimSpace(tokenClaims.Kubernetes.Namespace)
		}
		if podName == "" && tokenClaims.Kubernetes.Pod != nil {
			podName = strings.TrimSpace(tokenClaims.Kubernetes.Pod.Name)
		}
		if serviceAccount == "" && tokenClaims.Kubernetes.ServiceAccount != nil {
			serviceAccount = strings.TrimSpace(tokenClaims.Kubernetes.ServiceAccount.Name)
		}
	}

	kubernetesDetected := envValue(lookupEnv, "KUBERNETES_SERVICE_HOST") != "" ||
		strings.Contains(strings.ToUpper(executionEnvironment), "EKS") ||
		clusterName != "" ||
		namespaceErr == nil ||
		tokenErr == nil ||
		podName != "" ||
		serviceAccount != ""
	if !kubernetesDetected {
		return nil
	}

	if podName == "" {
		podName = strings.TrimSpace(envValue(lookupEnv, "HOSTNAME"))
	}

	metadata := &EKS{
		ClusterName:          strings.TrimSpace(clusterName),
		Namespace:            strings.TrimSpace(namespace),
		PodName:              strings.TrimSpace(podName),
		NodeName:             strings.TrimSpace(nodeName),
		ServiceAccount:       strings.TrimSpace(serviceAccount),
		ExecutionEnvironment: executionEnvironment,
	}
	if tokenClaims.Kubernetes != nil {
		if tokenClaims.Kubernetes.Pod != nil {
			metadata.PodUID = strings.TrimSpace(tokenClaims.Kubernetes.Pod.UID)
		}
		if tokenClaims.Kubernetes.ServiceAccount != nil {
			metadata.ServiceAccountUID = strings.TrimSpace(tokenClaims.Kubernetes.ServiceAccount.UID)
		}
	}
	if isZeroEKS(metadata) {
		return nil
	}
	return metadata
}

func readJSON(ctx context.Context, client *http.Client, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}

	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errors.New(response.Status)
	}

	limited := io.LimitReader(response.Body, maxMetadataBodyBytes)
	return json.NewDecoder(limited).Decode(target)
}

func decodeKubernetesToken(token []byte, target *kubernetesTokenClaims) error {
	parts := strings.Split(strings.TrimSpace(string(token)), ".")
	if len(parts) < 2 {
		return errors.New("kubernetes token is malformed")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, target)
}

func envValue(lookupEnv func(string) (string, bool), key string) string {
	if lookupEnv == nil {
		return ""
	}
	value, ok := lookupEnv(key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func firstNonEmptyEnv(lookupEnv func(string) (string, bool), keys ...string) string {
	for _, key := range keys {
		if value := envValue(lookupEnv, key); value != "" {
			return value
		}
	}
	return ""
}

func isZeroEC2(metadata *EC2) bool {
	return metadata == nil || *metadata == (EC2{})
}

func isZeroECS(metadata *ECS) bool {
	return metadata == nil || *metadata == (ECS{})
}

func isZeroEKS(metadata *EKS) bool {
	return metadata == nil || *metadata == (EKS{})
}
