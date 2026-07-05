// Command engram-deploy is the Phase-7 idempotent Go deploy CLI (D24 — no
// Terraform/HCL): it describes-then-converges the OpenSearch Service 3.1
// domain, the three ECS services (engramd, worker, the co-located BGE-M3
// embedding container), Secrets Manager, and the VPC for one environment
// (staging or prod), wrapped by `make deploy-staging`/`make deploy-prod`.
//
// Converge is idempotent: re-running it against an unchanged environment
// performs zero mutating AWS calls (proven in deploy/aws/awsapi's unit
// tests against a fake Provisioner — this binary wires the SAME Converge
// function to a real awsapi.SDKProvisioner, so the logic is identical,
// only the backend differs).
//
// Usage:
//
//	go run ./cmd/engram-deploy -env staging -image engram:v1.2.3
//	go run ./cmd/engram-deploy -env staging -rollback engramd
//
// This binary requires real AWS credentials (environment/shared config,
// standard aws-sdk-go-v2 resolution) and a real AWS account to do anything
// useful — in a build/CI environment without them, NewSDKProvisioner's
// underlying calls fail, which is the expected, documented behavior (see
// the Phase-7 report's manual-verification section).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ryanthedev/engram/deploy/aws/awsapi"
)

func main() {
	env := flag.String("env", "", "environment to converge: staging | prod")
	image := flag.String("image", "", "container image tag to deploy (required for converge, ignored for -rollback)")
	rollbackService := flag.String("rollback", "", "service name to roll back to its previous task-definition revision, instead of converging")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	target, err := environmentFor(*env, *image)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	provisioner, err := awsapi.NewSDKProvisioner(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error building AWS provisioner:", err)
		os.Exit(1)
	}

	if *rollbackService != "" {
		state, err := awsapi.Rollback(ctx, provisioner, target.Name, *rollbackService)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error rolling back:", err)
			os.Exit(1)
		}
		fmt.Printf("rolled back %s/%s to %s (desired count %d)\n", target.Name, *rollbackService, state.ActiveTaskDefinitionARN, state.DesiredCount)
		return
	}

	if *image == "" {
		fmt.Fprintln(os.Stderr, "error: -image is required to converge (or use -rollback <service>)")
		os.Exit(1)
	}
	rep, err := awsapi.Converge(ctx, provisioner, target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error converging:", err)
		os.Exit(1)
	}
	fmt.Printf("environment %s converged:\n", target.Name)
	for name, action := range rep.Actions {
		fmt.Printf("  %-40s %s\n", name, action)
	}
	if rep.Changed() {
		fmt.Println("result: resources created or updated")
	} else {
		fmt.Println("result: no-op (already up to date)")
	}
}

func environmentFor(env, image string) (awsapi.Environment, error) {
	switch env {
	case "staging":
		return stagingEnvironment(image), nil
	case "prod":
		return prodEnvironment(image), nil
	default:
		return awsapi.Environment{}, fmt.Errorf("unknown -env %q: want staging or prod", env)
	}
}
