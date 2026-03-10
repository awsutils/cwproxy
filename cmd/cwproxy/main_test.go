package main

import (
	"testing"

	"github.com/awsutils/cwproxy/internal/metadata"
)

func TestResolveAWSRegion(t *testing.T) {
	t.Run("uses AWS_REGION first", func(t *testing.T) {
		t.Setenv("AWS_REGION", "us-east-1")
		t.Setenv("AWS_DEFAULT_REGION", "us-west-2")

		region, source := resolveAWSRegion(&metadata.Snapshot{
			EC2: &metadata.EC2{Region: "ap-northeast-2"},
		})
		if region != "us-east-1" || source != "AWS_REGION" {
			t.Fatalf("resolveAWSRegion() = (%q, %q)", region, source)
		}
	})

	t.Run("uses AWS_DEFAULT_REGION when AWS_REGION is unset", func(t *testing.T) {
		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "us-west-2")

		region, source := resolveAWSRegion(&metadata.Snapshot{
			EC2: &metadata.EC2{Region: "ap-northeast-2"},
		})
		if region != "us-west-2" || source != "AWS_DEFAULT_REGION" {
			t.Fatalf("resolveAWSRegion() = (%q, %q)", region, source)
		}
	})

	t.Run("falls back to runtime metadata", func(t *testing.T) {
		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")

		region, source := resolveAWSRegion(&metadata.Snapshot{
			ECS: &metadata.ECS{
				TaskARN: "arn:aws:ecs:eu-west-1:123456789012:task/abc",
			},
		})
		if region != "eu-west-1" || source != "runtime metadata" {
			t.Fatalf("resolveAWSRegion() = (%q, %q)", region, source)
		}
	})

	t.Run("returns empty when nothing is available", func(t *testing.T) {
		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")

		region, source := resolveAWSRegion(nil)
		if region != "" || source != "" {
			t.Fatalf("resolveAWSRegion() = (%q, %q)", region, source)
		}
	})
}
