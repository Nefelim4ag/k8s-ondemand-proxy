package main

import (
	"context"
	"flag"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"nefelim4ag/k8s-ondemand-proxy/tcpserver"
)

type globalState struct {
	lastServe  atomic.Int64
	upsreamSrv *net.TCPAddr
	namespace  string
	group      string
	name       string
	client *clientset.Clientset
}

func (state *globalState) touch() {
	state.lastServe.Store(time.Now().UnixMicro())
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func (state *globalState) pipe(src *net.TCPConn, dst *net.TCPConn) {
	defer src.Close()
	defer dst.Close()
	buf := make([]byte, 4*4096)

	for {
		state.touch()
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		b := buf[:n]

		state.touch()
		n, err = dst.Write(b)
		if err != nil {
			return
		}
	}
}

func (state *globalState) connectionHandler(clientConn *net.TCPConn, err error) {
	if err != nil {
		slog.Error(err.Error())
		return
	}
	state.touch()

	serverConn, err := net.DialTCP("tcp", nil, state.upsreamSrv)
	if err != nil {
		slog.Error(err.Error())
		return
	}
	slog.Info("Handle connection", "client", clientConn.RemoteAddr().String(), "server", serverConn.RemoteAddr().String())

	// Handle connection close internal in pipe, close both ends in same time
	go state.pipe(clientConn, serverConn)
	state.pipe(serverConn, clientConn)
}

func (state *globalState) updateScale(replicas int32) {
	client := state.client
	switch state.group {
	case "statefulset", "sts":
		statefulSetClient := client.AppsV1().StatefulSets(state.namespace)
		scale, err := statefulSetClient.GetScale(context.TODO(), state.name, metav1.GetOptions{})
		if err != nil {
			return
		}
		scale.Spec.Replicas = replicas
		scale, err = statefulSetClient.UpdateScale(context.TODO(), state.name, scale, metav1.UpdateOptions{})
		if err != nil {
			slog.Error(err.Error())
			return
		}
	case "deployment", "deploy":
		deploymentClient := client.AppsV1().Deployments(state.namespace)
		scale, err := deploymentClient.GetScale(context.TODO(), state.name, metav1.GetOptions{})
		if err != nil {
			return
		}
		scale.Spec.Replicas = replicas
		scale, err = deploymentClient.UpdateScale(context.TODO(), state.name, scale, metav1.UpdateOptions{})
		if err != nil {
			slog.Error(err.Error())
			return
		}
	default:
		slog.Error("Api group not supported", "api", state.group)
	}
}

func main() {
	var kubeconfig string
	var rawUpstreamServerAddr string
	var rawLocalServerAddr string
	var namespace string
	var resourceName string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&rawUpstreamServerAddr, "upstream", "", "Remote server address like dind.ci.svc.cluster.local:2375")
	flag.StringVar(&rawLocalServerAddr, "listen", "", "Local address listen to like :2375")
	flag.StringVar(&namespace, "namespace", "", "Kubernetes namespace to work with")
	flag.StringVar(&resourceName, "resource-name", "", "Kubernetes resource like deployment/app")
	replicas := flag.Int64("replicas", 1, "replica count on traffic & on cold startup")
	flag.Parse()

	programLevel := new(slog.LevelVar)
	logger := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: programLevel})
	slog.SetDefault(slog.New(logger))

	config, err := buildConfig(kubeconfig)
	if err != nil {
		slog.Error(err.Error())
	}
	client := clientset.NewForConfigOrDie(config)

	state := globalState{
		client: client,
		namespace: namespace,
	}
	state.upsreamSrv, err = net.ResolveTCPAddr("tcp", rawUpstreamServerAddr)
	if err != nil {
		slog.Error("failed to resolve address", rawUpstreamServerAddr, err.Error())
		return
	}

	resourceArgs := strings.Split(resourceName, "/")
	if len(resourceArgs) != 2 {
		slog.Error("Wrong resource name, must be statefulset/app or deployment/app", "parsed", resourceArgs)
		return
	}
	state.group = resourceArgs[0]
	state.name = resourceArgs[1]
	state.updateScale(int32(*replicas))

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	srvInstance := tcpserver.Server{}
	err = srvInstance.ListenAndServe(rawLocalServerAddr, state.connectionHandler)
	if err != nil {
		slog.Error(err.Error())
	}

	<-sigChan
	slog.Info("Shutting down server...")
	srvInstance.Stop()
	slog.Info("Server stopped.")
}
