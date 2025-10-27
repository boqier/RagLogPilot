/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	logv1 "github.com/boqier/RagLogPilot/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// RagLogPilotReconciler reconciles a RagLogPilot object
type RagLogPilotReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	KubeClient *kubernetes.Clientset
}

// +kubebuilder:rbac:groups=log.aiops.com,resources=raglogpilots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=log.aiops.com,resources=raglogpilots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=log.aiops.com,resources=raglogpilots/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the RagLogPilot object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *RagLogPilotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	//获取日志
	var raglogPilot logv1.RagLogPilot
	if err := r.Get(ctx, req.NamespacedName, &raglogPilot); err != nil {
		logger.Error(err, "unable to fetch RagLogPilot")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	//检查是否有coversationId
	if raglogPilot.Status.ConversationId == "" {
		conversationId, err := r.createConversation(raglogPilot)
		if err != nil {
			logger.Error(err, "unable to create conversationId")
			return ctrl.Result{}, err
		}
		raglogPilot.Status.ConversationId = conversationId
		if err := r.Status().Update(ctx, &raglogPilot); err != nil {
			logger.Error(err, "unable to update RagLogPilot status")
			return ctrl.Result{}, err
		}
	}
	//获取命名空间下的所有pod
	var pods corev1.PodList
	if err := r.List(ctx, &pods, &client.ListOptions{Namespace: req.Namespace}); err != nil {
		logger.Error(err, "unable to list pods")
		return ctrl.Result{}, err
	}
	for _, pod := range pods.Items {
		logString, err := r.getPodLogs(pod)
		if err != nil {
			logger.Error(err, "unable to get pod logs", "pod", pod.Name)
			continue
		}
		var errorLogs []string
		logLines := strings.Split(logString, "\n")
		for _, line := range logLines {
			if strings.Contains(line, "ERROR") {
				errorLogs = append(errorLogs, line)
			}
		}
		if len(errorLogs) > 0 {
			//发送给知识库检索
			combinedErrorLogs := strings.Join(errorLogs, "\n")
			fmt.Println("combinedErrorLog:", combinedErrorLogs)
			answer, err := r.queryRagSystem(combinedErrorLogs, raglogPilot)
			if err != nil {
				logger.Error(err, "unable to query Rag System", "pod", pod.Name)
				return ctrl.Result{}, err
			}
			//发送飞书
			err = r.sendFeishuAlter(raglogPilot.Spec.FeishuWebhook, answer)
			if err != nil {
				logger.Error(err, "unable to send Feishu alert", "pod", pod.Name)
			}
			logger.Info("rag system response", "answer", answer)
		}
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// ragflow查询
func (r *RagLogPilotReconciler) queryRagSystem(podLog string, ragLogPilot logv1.RagLogPilot) (string, error) {
	model := "Pro/Qwen/Qwen2.5-7B-Instructqianwen"
	payload := map[string]interface{}{
		"model": model, // 模型名，示例可用 "gpt-4" 或实际 RagFlow 模型名
		"messages": []map[string]string{
			{
				"role":    "user",
				"content": fmt.Sprintf("以下是获取到的日志：%s，请基于运维知识库进行解答，如果你不知道，就说不知道。", podLog),
			},
		},
		"stream": false,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/api/v1/chats_openai/%s/chat/completions", ragLogPilot.Spec.RagFlowEndpoint, ragLogPilot.Status.ConversationId)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", ragLogPilot.Spec.RagFlowToken))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	fmt.Println(resp.StatusCode)

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	// 安全地逐层取值
	choices, ok := result["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "", fmt.Errorf("no choices found in response")
	}

	firstChoice, ok := choices[0].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("invalid choices[0] structure")
	}

	delta, ok := firstChoice["message"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("no delta found in choice")
	}

	content, ok := delta["content"].(string)
	if !ok {
		return "", fmt.Errorf("no content found in delta")
	}

	return content, nil

}

// 发送飞书告警
func (r *RagLogPilotReconciler) sendFeishuAlter(webhook, analysis string) error {
	message := map[string]interface{}{
		"msg_type": "text",
		"content": map[string]string{
			"text": analysis,
		},
	}
	messageBody, err := json.Marshal(message)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", webhook, bytes.NewBuffer(messageBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to send  fieshu alert,status code :%d", resp.StatusCode)
	}
	return nil
}

func (r *RagLogPilotReconciler) createConversation(raglogPilot logv1.RagLogPilot) (string, error) {
	// 构造请求体
	payload := map[string]string{
		"name": "new session",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	// 创建请求
	url := fmt.Sprintf("%s/api/v1/chats/%s/sessions", raglogPilot.Spec.RagFlowEndpoint, "52409f92b31311f0875a6a2d3effa18a")
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// 设置头部
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", raglogPilot.Spec.RagFlowToken))

	// 发送请求
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// 检查响应码
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("unexpected response: %d - %s", resp.StatusCode, string(bodyBytes))
	}

	// 解析响应体
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	chatID, ok := result["data"].(map[string]interface{})["chat_id"].(string)
	if !ok {
		return "", fmt.Errorf("chat_id not found in response")
	}

	fmt.Println("获得的 chat_id 是：", chatID)
	return chatID, nil
}

func (r *RagLogPilotReconciler) getPodLogs(pod corev1.Pod) (string, error) {
	tailLines := int64(20)
	logOptions := &corev1.PodLogOptions{TailLines: &tailLines}
	req := r.KubeClient.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, logOptions)
	logStream, err := req.Stream(context.TODO())
	if err != nil {
		return "", err
	}
	defer logStream.Close()
	var logBuffer bytes.Buffer
	if _, err := logBuffer.ReadFrom(logStream); err != nil {
		return "", err
	}
	return logBuffer.String(), err
}

// SetupWithManager sets up the controller with the Manager.
func (r *RagLogPilotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	var kubeConfig *string
	var config *rest.Config
	if home := homedir.HomeDir(); home != "" {
		kubeConfig = flag.String("kubeConfig", filepath.Join(home, ".kube", "config"), "[可选]绝对路径")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		if config, err = clientcmd.BuildConfigFromFlags("", *kubeConfig); err != nil {
			return err
		}
	}
	r.KubeClient, err = kubernetes.NewForConfig(config)
	return ctrl.NewControllerManagedBy(mgr).
		For(&logv1.RagLogPilot{}).
		Named("raglogpilot").
		Complete(r)
}
