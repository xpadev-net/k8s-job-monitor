package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/xpadev/k8s-job-monitor/pkg/controller"
)

func main() {
	var kubeconfig string
	var masterURL string
	var webhookURL string
	var jobNameFilter string
	var includeMatches bool

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&webhookURL, "webhook-url", "", "URL to send webhook notifications")
	flag.StringVar(&jobNameFilter, "job-name-filter", "", "Regular expression pattern to filter jobs by name")
	flag.BoolVar(&includeMatches, "include-matches", true, "If true, include jobs matching the filter pattern; if false, exclude them")
	flag.Parse()

	if webhookURL == "" {
		klog.Fatal("webhook-url is required")
	}

	// klogの設定
	klog.InitFlags(nil)
	flag.Set("v", "2") // ログレベルを設定

	// クラスター内かクラスター外かに応じてKubernetesクライアント設定を取得
	var config *rest.Config
	var err error

	if kubeconfig == "" {
		klog.Info("Using in-cluster configuration")
		config, err = rest.InClusterConfig()
	} else {
		klog.Infof("Using kubeconfig: %s", kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	}
	if err != nil {
		klog.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	// Kubernetesクライアントセットを作成
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	// インフォーマーファクトリーを作成
	informerFactory := informers.NewSharedInformerFactory(kubeClient, time.Second*30)

	// Jobコントローラーを初期化
	jobController, err := controller.NewJobController(
		kubeClient,
		informerFactory.Batch().V1().Jobs(),
		webhookURL,
		jobNameFilter,
		includeMatches,
	)
	if err != nil {
		klog.Fatalf("Error initializing job controller: %s", err.Error())
	}

	// インフォーマーファクトリーを開始
	stopCh := SetupSignalHandler()
	informerFactory.Start(stopCh)

	// コントローラーを実行
	if err = jobController.Run(2, stopCh); err != nil {
		klog.Fatalf("Error running controller: %s", err.Error())
	}
}

// SetupSignalHandler はプロセスのシャットダウンシグナルを処理するストップチャネルを返します
func SetupSignalHandler() <-chan struct{} {
	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		close(stopCh)
		<-sigCh
		os.Exit(1) // 2回目のシグナルを受け取ったら強制終了
	}()
	return stopCh
}
