package storageclass

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/mcp/tools"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func DiagnoseCSIDriverTool() mcp.Tool {
	return mcp.NewTool(
		"diagnose_k8s_csi_driver",
		mcp.WithDescription("针对指定的PVC，诊断其CSI存储驱动和挂载卷绑定状态。包括获取VolumeAttachment状态与CSI控制器Pod异常。 / Diagnose CSI driver and VolumeAttachment bind status for a given PVC."),
		mcp.WithTitleAnnotation("Diagnose CSI Storage"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("cluster", mcp.Required(), mcp.Description("集群名称/Cluster name")),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("命名空间 / Namespace")),
		mcp.WithString("pvc_name", mcp.Required(), mcp.Description("PVC 名称 / PVC name")),
	)
}

func DiagnoseCSIDriverHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, meta, err := tools.ParseFromRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	pvcName := request.GetString("pvc_name", "")
	if pvcName == "" {
		return nil, fmt.Errorf("pvc_name is required")
	}

	kubectl := kom.Cluster(meta.Cluster).WithContext(ctx)
	clientset := kubectl.Client()
	if clientset == nil {
		return nil, fmt.Errorf("failed to get kubernetes clientset")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("--- CSI Diagnostics for PVC %s/%s ---\n", meta.Namespace, pvcName))

	// 1. Get PVC
	pvc, err := clientset.CoreV1().PersistentVolumeClaims(meta.Namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get PVC %s: %v", pvcName, err)
	}

	sb.WriteString(fmt.Sprintf("PVC Status: %s\n", pvc.Status.Phase))
	if pvc.Spec.StorageClassName != nil {
		sb.WriteString(fmt.Sprintf("StorageClass: %s\n", *pvc.Spec.StorageClassName))
	} else {
		sb.WriteString("StorageClass: (none/default)\n")
	}
	sb.WriteString(fmt.Sprintf("VolumeName (PV): %s\n", pvc.Spec.VolumeName))

	// 2. Fetch StorageClass details to find provisioner
	provisioner := ""
	if pvc.Spec.StorageClassName != nil {
		sc, scErr := clientset.StorageV1().StorageClasses().Get(ctx, *pvc.Spec.StorageClassName, metav1.GetOptions{})
		if scErr == nil {
			provisioner = sc.Provisioner
			sb.WriteString(fmt.Sprintf("SC Provisioner: %s\n", provisioner))
		} else {
			sb.WriteString(fmt.Sprintf("Warning: Failed to fetch StorageClass %s: %v\n", *pvc.Spec.StorageClassName, scErr))
		}
	}

	// 3. Find VolumeAttachment for PV
	pvName := pvc.Spec.VolumeName
	if pvName != "" {
		sb.WriteString("\n--- VolumeAttachment Status ---\n")
		vas, vaErr := clientset.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
		if vaErr == nil {
			foundVa := false
			for _, va := range vas.Items {
				if va.Spec.Source.PersistentVolumeName != nil && *va.Spec.Source.PersistentVolumeName == pvName {
					foundVa = true
					sb.WriteString(fmt.Sprintf("VolumeAttachment: %s\n", va.Name))
					sb.WriteString(fmt.Sprintf("  NodeName: %s\n", va.Spec.NodeName))
					sb.WriteString(fmt.Sprintf("  Attacher: %s\n", va.Spec.Attacher))
					sb.WriteString(fmt.Sprintf("  Attached: %v\n", va.Status.Attached))
					if va.Status.AttachError != nil {
						sb.WriteString(fmt.Sprintf("  AttachError: %s\n", va.Status.AttachError.Message))
					}
					if va.Status.DetachError != nil {
						sb.WriteString(fmt.Sprintf("  DetachError: %s\n", va.Status.DetachError.Message))
					}
				}
			}
			if !foundVa {
				sb.WriteString(fmt.Sprintf("No active VolumeAttachment found matching PV %s.\n", pvName))
			}
		} else {
			sb.WriteString(fmt.Sprintf("Warning: Failed to list VolumeAttachments: %v\n", vaErr))
		}
	}

	// 4. Find CSI driver controller Pods in common namespaces
	if provisioner != "" {
		sb.WriteString("\n--- CSI Controller Pods ---\n")
		// Common CSI driver namespaces
		csiNamespaces := []string{"kube-system", "longhorn-system", "rook-ceph", "kube-storage", meta.Namespace}
		foundPods := false

		// Extract short name of provisioner for searching (e.g. ebs.csi.aws.com -> ebs)
		parts := strings.Split(provisioner, ".")
		shortName := provisioner
		if len(parts) > 0 {
			shortName = parts[0]
		}

		for _, ns := range csiNamespaces {
			pods, listErr := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
			if listErr != nil {
				continue
			}

			for _, pod := range pods.Items {
				podNameLower := strings.ToLower(pod.Name)
				// Match pod name by provisioner key or CSI keywords
				if strings.Contains(podNameLower, "csi") && (strings.Contains(podNameLower, shortName) || strings.Contains(podNameLower, "controller") || strings.Contains(podNameLower, "provisioner")) {
					foundPods = true
					sb.WriteString(fmt.Sprintf("Namespace: %s, Pod: %s, Status: %s, Ready: %v\n",
						ns, pod.Name, pod.Status.Phase, isPodReady(&pod)))
				}
			}
		}

		if !foundPods {
			sb.WriteString("No matching CSI controller pods found in standard namespaces.\n")
		}
	}

	return tools.TextResult(sb.String(), meta)
}
