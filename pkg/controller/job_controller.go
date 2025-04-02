package controller

import (
	"fmt"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	batchinformers "k8s.io/client-go/informers/batch/v1"
	"k8s.io/client-go/kubernetes"
	batchlisters "k8s.io/client-go/listers/batch/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/xpadev/k8s-job-monitor/pkg/webhook"
)

// JobController はJobリソースの監視と通知を行うコントローラーです
type JobController struct {
	// kubeclientset はKubernetesクライアントセットです
	kubeclientset kubernetes.Interface
	// jobLister はJobs APIへの読み取り専用アクセスを提供します
	jobLister batchlisters.JobLister
	// jobsSynced はJobのキャッシュが同期されたかどうかを示します
	jobsSynced cache.InformerSynced
	// workqueue はコントローラーが処理を行うJobのキーを追跡します
	workqueue workqueue.RateLimitingInterface
	// webhook はWebhook通知クライアントです
	webhook *webhook.WebhookClient
	// notifiedJobs は通知が既に送信されたJobを追跡します
	notifiedJobs     map[string]bool
	notifiedJobsLock sync.RWMutex
}

// NewJobController は新しいJobControllerを作成します
func NewJobController(
	kubeclientset kubernetes.Interface,
	jobInformer batchinformers.JobInformer,
	webhookURL string) *JobController {

	controller := &JobController{
		kubeclientset: kubeclientset,
		jobLister:     jobInformer.Lister(),
		jobsSynced:    jobInformer.Informer().HasSynced,
		workqueue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Jobs"),
		webhook:       webhook.NewWebhookClient(webhookURL),
		notifiedJobs:  make(map[string]bool),
	}

	klog.Info("Setting up event handlers")
	// Jobの変更を監視するイベントハンドラを追加
	jobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueJob,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueJob(new)
		},
	})

	return controller
}

// Run はコントローラーを実行します
func (c *JobController) Run(workers int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	// コントローラーの起動ログ
	klog.Info("Starting Job controller")

	// インフォーマーのキャッシュが同期されるのを待ちます
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.jobsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	// 複数のワーカーを起動
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	klog.Info("Started workers")
	<-stopCh
	klog.Info("Shutting down workers")

	return nil
}

// runWorker はワーカーのメインループを定義します
func (c *JobController) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem はワークキューから次の作業項目を取り出して処理します
func (c *JobController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// この関数が終了したらDoneを呼び出すことを確認します
	defer c.workqueue.Done(obj)

	// オブジェクトからキーを取得
	var key string
	var ok bool
	if key, ok = obj.(string); !ok {
		c.workqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
		return true
	}

	// syncHandlerを実行して実際の同期を行う
	if err := c.syncHandler(key); err != nil {
		// 項目を再キューに入れる
		c.workqueue.AddRateLimited(key)
		return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error()) != nil
	}

	// 項目の処理が成功した場合、キャッシュから削除
	c.workqueue.Forget(obj)
	klog.Infof("Successfully synced '%s'", key)
	return true
}

// isJobAlreadyNotified はJobが既に通知されたかどうかを確認します
func (c *JobController) isJobAlreadyNotified(key string) bool {
	c.notifiedJobsLock.RLock()
	defer c.notifiedJobsLock.RUnlock()
	return c.notifiedJobs[key]
}

// markJobNotified はJobを通知済みとしてマークします
func (c *JobController) markJobNotified(key string) {
	c.notifiedJobsLock.Lock()
	defer c.notifiedJobsLock.Unlock()
	c.notifiedJobs[key] = true
}

// syncHandler はJobの現在の状態を取得し、必要に応じてWebhook通知を送信します
func (c *JobController) syncHandler(key string) error {
	// キーからnamespaceとnameを取得
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// 既に通知済みの場合は何もしない
	if c.isJobAlreadyNotified(key) {
		klog.V(4).Infof("Job '%s' has already been notified, skipping", key)
		return nil
	}

	// JobをListerから取得
	job, err := c.jobLister.Jobs(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// 削除されたJobs（バキュームによって）に対してはスキップ
			runtime.HandleError(fmt.Errorf("job '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}

	// Jobのステータスをチェックして通知を送信
	var notified bool
	if isJobCompleted(job) {
		if err = c.sendJobCompletedNotification(job); err != nil {
			return err
		}
		notified = true
	} else if isJobFailed(job) {
		if err = c.sendJobFailedNotification(job); err != nil {
			return err
		}
		notified = true
	}

	// 通知を送信した場合は通知済みとしてマーク
	if notified {
		c.markJobNotified(key)
		klog.Infof("Sent notification for job '%s'", key)
	}

	return nil
}

// isJobCompleted はJobが完了したかどうかを判定します
func isJobCompleted(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == "True" {
			return true
		}
	}
	return false
}

// isJobFailed はJobが失敗したかどうかを判定します
func isJobFailed(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == "True" {
			return true
		}
	}
	return false
}

// sendJobCompletedNotification はJobが正常に完了したときの通知を送信します
func (c *JobController) sendJobCompletedNotification(job *batchv1.Job) error {
	notification := webhook.JobNotification{
		Name:      job.Name,
		Namespace: job.Namespace,
		Status:    "Completed",
		Message:   fmt.Sprintf("Job %s/%s completed successfully", job.Namespace, job.Name),
	}

	return c.webhook.SendNotification(notification)
}

// sendJobFailedNotification はJobが失敗したときの通知を送信します
func (c *JobController) sendJobFailedNotification(job *batchv1.Job) error {
	notification := webhook.JobNotification{
		Name:      job.Name,
		Namespace: job.Namespace,
		Status:    "Failed",
		Message:   fmt.Sprintf("Job %s/%s failed", job.Namespace, job.Name),
	}

	return c.webhook.SendNotification(notification)
}

// enqueueJob はJobをコントローラーのワークキューに追加します
func (c *JobController) enqueueJob(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
