package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/baizeai/kube-snapshot/api/v1alpha1"
	criruntime "github.com/baizeai/kube-snapshot/internal/controller/runtime"
)

type registryCommand struct {
	Args   []string
	Config string
}

// Run the test binary as a recording docker/nerdctl executable. This also works
// on Windows, where a shell script cannot be executed directly by exec.Command.
func init() {
	if os.Getenv("KUBE_SNAPSHOT_COMMAND_HELPER") != "1" {
		return
	}
	invocation := registryCommand{Args: os.Args[1:]}
	if configDir := os.Getenv("DOCKER_CONFIG"); configDir != "" {
		contents, err := os.ReadFile(filepath.Join(configDir, "config.json"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		invocation.Config = string(contents)
	}
	trace, err := os.OpenFile(os.Getenv("KUBE_SNAPSHOT_COMMAND_TRACE"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	err = json.NewEncoder(trace).Encode(invocation)
	trace.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if fail := os.Getenv("KUBE_SNAPSHOT_FAIL_COMMAND"); fail != "" {
		for _, arg := range invocation.Args {
			if arg == fail {
				os.Exit(1)
			}
		}
	}
	os.Exit(0)
}

func TestSnapshotRegistryCommands(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := snapshotv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const (
		targetRegistry = "registry.example.test:5000"
		originRegistry = "origin.example.test:5443"
		targetImage    = targetRegistry + "/team/snapshot:v1"
		originImage    = originRegistry + "/base:v1"
		targetConfig   = `{"auths":{"registry.example.test:5000":{"auth":"dXNlcjpwYXNz"}}}`
		originConfig   = `{"auths":{"origin.example.test:5443":{"auth":"b3JpZ2luOnBhc3M="}}}`
	)
	secret := func(name, config string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(config)},
		}
	}
	r := &SnapshotPodTaskReconciler{
		Scheme: scheme,
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			secret("target", targetConfig), secret("origin", originConfig),
		).Build(),
	}
	// Use the production runtime wiring without contacting a containerd socket.
	// The containerd runtime delegates registry operations to the same wrapper.
	if err := r.setupRuntimes(func(cmd string, args ...string) (criruntime.Runtime, error) {
		return criruntime.NewDockerCompatibleRuntime(cmd, args...), nil
	}); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	for _, name := range []string{"docker", "nerdctl"} {
		if goruntime.GOOS == "windows" {
			name += ".exe"
		}
		target := filepath.Join(binDir, name)
		if err := os.Link(executable, target); err != nil {
			contents, err := os.ReadFile(executable)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, contents, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KUBE_SNAPSHOT_COMMAND_HELPER", "1")
	inspect := registryCommand{Args: []string{"image", "inspect", targetImage}}
	pull := registryCommand{Args: []string{"pull", originImage, "--unpack=false"}}
	login := registryCommand{Args: []string{"login", targetRegistry}, Config: targetConfig}
	push := registryCommand{Args: []string{"push", targetImage}}
	for _, tt := range []struct {
		name, runtime, targetSecret, originSecret, fail, errorContains string
		commands                                                       []registryCommand
	}{
		{"anonymous containerd", containerdRuntime, "", "", "", "", []registryCommand{inspect, pull, push}},
		{"separate source and target credentials", containerdRuntime, "target", "origin", "", "", []registryCommand{
			inspect,
			{Args: []string{"login", originRegistry}, Config: originConfig},
			{Args: pull.Args, Config: originConfig},
			login,
			{Args: push.Args, Config: targetConfig},
		}},
		{"login failure stops push", containerdRuntime, "target", "", "login", "login '" + targetRegistry + "'", []registryCommand{inspect, pull, login}},
		{"push failure does not retry anonymously", containerdRuntime, "target", "", "push", "after login", []registryCommand{
			inspect, pull, login, {Args: push.Args, Config: targetConfig},
		}},
		{"source pull failure remains nonfatal", containerdRuntime, "target", "", "pull", "", []registryCommand{
			inspect, pull, login, {Args: push.Args, Config: targetConfig},
		}},
		{"anonymous Docker unchanged", dockerRuntime, "", "", "", "", []registryCommand{inspect, push}},
		{"authenticated Docker unchanged", dockerRuntime, "target", "", "", "", []registryCommand{
			inspect, login, {Args: push.Args, Config: targetConfig},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "commands.jsonl")
			t.Setenv("KUBE_SNAPSHOT_COMMAND_TRACE", trace)
			t.Setenv("KUBE_SNAPSHOT_FAIL_COMMAND", tt.fail)
			t.Setenv("DOCKER_CONFIG", "")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := r.reconcilePushImage(ctx, &snapshotv1alpha1.SnapshotPodTask{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: snapshotv1alpha1.SnapshotPodTaskSpec{
					ContainerID:             tt.runtime + "://container",
					CommitImage:             targetImage,
					OriginImage:             originImage,
					RegistrySecretRef:       tt.targetSecret,
					OriginRegistrySecretRef: tt.originSecret,
				},
			})
			if tt.errorContains == "" && err != nil {
				t.Fatal(err)
			}
			if tt.errorContains != "" && (err == nil || !strings.Contains(err.Error(), tt.errorContains)) {
				t.Fatalf("expected error containing %q, got %v", tt.errorContains, err)
			}
			file, err := os.Open(trace)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var actual []registryCommand
			decoder := json.NewDecoder(file)
			for {
				var command registryCommand
				if err := decoder.Decode(&command); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				actual = append(actual, command)
			}
			prefix := []string{"--debug"}
			if tt.runtime == containerdRuntime {
				prefix = append(prefix, "-n", "k8s.io", "--insecure-registry")
			}
			var expected []registryCommand
			for _, command := range tt.commands {
				expected = append(expected, registryCommand{
					Args: append(append([]string{}, prefix...), command.Args...), Config: command.Config,
				})
			}
			if !reflect.DeepEqual(expected, actual) {
				t.Fatalf("unexpected commands or credentials\nexpected: %#v\nactual: %#v", expected, actual)
			}
		})
	}

	t.Run("Docker commit options precede positional arguments", func(t *testing.T) {
		trace := filepath.Join(t.TempDir(), "commands.jsonl")
		t.Setenv("KUBE_SNAPSHOT_COMMAND_TRACE", trace)
		t.Setenv("KUBE_SNAPSHOT_FAIL_COMMAND", "")
		t.Setenv("DOCKER_CONFIG", "")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.runtimeMap[dockerRuntime].Commit(ctx, "container-id", targetImage, false, "snapshot", "notebook"); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(trace)
		if err != nil {
			t.Fatal(err)
		}
		var command registryCommand
		if err := json.Unmarshal(contents, &command); err != nil {
			t.Fatal(err)
		}
		expected := []string{"--debug", "commit", "--pause=false", "--message", "snapshot", "--author", "notebook", "container-id", targetImage}
		if !reflect.DeepEqual(command.Args, expected) {
			t.Fatalf("expected Docker commit arguments %v, got %v", expected, command.Args)
		}
	})
}
