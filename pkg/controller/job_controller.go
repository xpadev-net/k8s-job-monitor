package controller

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// startTime はコントローラーの起動時刻です
	startTime time.Time
	// jobNameFilterRegexp はジョブ名のフィルタリングに使用する正規表現パターンです
	jobNameFilterRegexp *regexp.Regexp
	// jobNameFilterInclude はフィルタパターンが含む(true)か除外(false)かを示します
	jobNameFilterInclude bool
}

// NewJobController は新しいJobControllerを作成します
func NewJobController(
	kubeclientset kubernetes.Interface,
	jobInformer batchinformers.JobInformer,
	webhookURL string,
	jobNameFilter string,
	includeMatches bool) (*JobController, error) {

	var jobNameFilterRegexp *regexp.Regexp
	var err error
	
	if jobNameFilter != "" {
		// 正規表現パターンをコンパイル
		jobNameFilterRegexp, err = regexp.Compile(jobNameFilter)
		if err != nil {
			return nil, fmt.Errorf("invalid job name filter pattern: %w", err)
		}
		klog.Infof("Using job name filter: %s (include matches: %v)", jobNameFilter, includeMatches)
	} else {
		klog.Info("No job name filter specified, will process all jobs")
	}

	controller := &JobController{
		kubeclientset:       kubeclientset,
		jobLister:           jobInformer.Lister(),
		jobsSynced:          jobInformer.Informer().HasSynced,
		workqueue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Jobs"),
		webhook:             webhook.NewWebhookClient(webhookURL),
		notifiedJobs:        make(map[string]bool),
		startTime:           time.Now(),
		jobNameFilterRegexp: jobNameFilterRegexp,
		jobNameFilterInclude: includeMatches,
	}

	klog.Info("Setting up event handlers")
	// Jobの変更を監視するイベントハンドラを追加
	jobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueJob,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueJob(new)
		},
	})

	return controller, nil
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
		runtime.HandleError(fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error()))
		return true
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

// shouldNotifyForJob はジョブが通知対象かどうかを判定します
func (c *JobController) shouldNotifyForJob(job *batchv1.Job) bool {
	// フィルターが設定されていない場合は、すべてのジョブを通知
	if c.jobNameFilterRegexp == nil {
		return true
	}

	// ジョブ名がパターンにマッチするかチェック
	matches := c.jobNameFilterRegexp.MatchString(job.Name)

	// includeMatchesがtrueの場合、マッチしたものを含める
	// falseの場合、マッチしたものを除外
	return matches == c.jobNameFilterInclude
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
				// ジョブが見つからない場合は、kubeclientを使用して最後の状態を取得しようとする
				klog.V(2).Infof("Job '%s' not found in cache, trying to get from API", key)
				job, err = c.kubeclientset.BatchV1().Jobs(namespace).Get(context.Background(), name, metav1.GetOptions{})
				if err != nil {
					if errors.IsNotFound(err) {
						// ジョブが完全に削除された場合は通知の必要なし
						klog.V(2).Infof("Job '%s' completely deleted, skipping notification", key)
						c.workqueue.Forget(key) // キューから削除
						return nil
					}
					// その他のAPIエラーは返す
					return err
				}
				// ジョブが見つかった場合、処理を続行
			} else {
				// その他のエラーは返す
				return err
			}
	}

	// ジョブ名でフィルタリング
	if !c.shouldNotifyForJob(job) {
		klog.V(3).Infof("Job '%s/%s' filtered out by name pattern, skipping notification", 
			job.Namespace, job.Name)
		c.markJobNotified(key) // フィルタリングされたジョブも通知済みとしてマーク
		return nil
	}

	// ジョブがコントローラー起動前に完了した場合はスキップ
	if c.isJobCompletedBeforeStart(job) {
		klog.V(2).Infof("Job '%s/%s' completed before controller started, skipping notification", job.Namespace, job.Name)
		c.markJobNotified(key) // このジョブは通知済みとしてマークしておく（再処理を避けるため）
		return nil
	}

	// ログに詳細なジョブ状態を出力
	klog.V(3).Infof("Processing job '%s/%s', status: completed=%v, failed=%v, active=%d, succeeded=%d, failed=%d",
		job.Namespace, job.Name, isJobCompleted(job), isJobFailed(job),
		job.Status.Active, job.Status.Succeeded, job.Status.Failed)

	// Jobのステータスをチェックして通知を送信
	var notified bool
	if isJobCompleted(job) {
		klog.V(2).Infof("Job '%s/%s' is completed, sending notification", job.Namespace, job.Name)
		if err = c.sendJobCompletedNotification(job); err != nil {
			return fmt.Errorf("failed to send completion notification: %w", err)
		}
		notified = true
	} else if isJobFailed(job) {
		klog.V(2).Infof("Job '%s/%s' is failed, sending notification", job.Namespace, job.Name)
		if err = c.sendJobFailedNotification(job); err != nil {
			return fmt.Errorf("failed to send failure notification: %w", err)
		}
		notified = true
	} else {
		// ジョブはまだ進行中か、完了/失敗の条件がセットされていない
		// 追加のチェック: Kubernetes JobコントローラがConditionsをセットしない場合がある
		if job.Status.Failed > 0 && job.Status.Active == 0 {
			klog.V(2).Infof("Job '%s/%s' has failures and no active pods, sending failure notification", job.Namespace, job.Name)
			if err = c.sendJobFailedNotification(job); err != nil {
				return fmt.Errorf("failed to send failure notification: %w", err)
			}
			notified = true
		} else if job.Status.Succeeded > 0 && job.Status.Active == 0 {
			klog.V(2).Infof("Job '%s/%s' has succeeded pods and no active pods, sending completion notification", job.Namespace, job.Name)
			if err = c.sendJobCompletedNotification(job); err != nil {
				return fmt.Errorf("failed to send completion notification: %w", err)
			}
			notified = true
		}
	}

	// 通知を送信した場合は通知済みとしてマーク
	if notified {
		c.markJobNotified(key)
		klog.Infof("Sent notification for job '%s'", key)
	} else {
		klog.V(4).Infof("Job '%s/%s' is not completed or failed yet, skipping notification", job.Namespace, job.Name)
	}

	return nil
}

// isJobCompletedBeforeStart はジョブがコントローラー起動前に完了したかを判定します
func (c *JobController) isJobCompletedBeforeStart(job *batchv1.Job) bool {
	// 終了時刻を取得
	var completionTime time.Time
	
	// 完了条件から時刻を取得
	for _, condition := range job.Status.Conditions {
		if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) && 
		   condition.Status == "True" && 
		   !condition.LastTransitionTime.IsZero() {
			completionTime = condition.LastTransitionTime.Time
			break
		}
	}
	
	// 条件から終了時刻が取得できない場合、CompletionTimeを確認
	if completionTime.IsZero() && job.Status.CompletionTime != nil {
		completionTime = job.Status.CompletionTime.Time
	}
	
	// それでも終了時刻が不明な場合は、コントローラー起動後とみなす
	if completionTime.IsZero() {
		return false
	}
	
	// 終了時刻がコントローラー起動時刻より前かどうかを判定
	return completionTime.Before(c.startTime)
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
