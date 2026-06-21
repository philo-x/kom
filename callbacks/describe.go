package callbacks

import (
	"fmt"

	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/kom/describe"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

func Describe(k *kom.Kubectl) error {

	stmt := k.Statement
	ns := stmt.Namespace
	name := stmt.Name
	gvk := k.Statement.GVK
	namespaced := stmt.Namespaced

	if stmt.GVK.Empty() {
		return fmt.Errorf("请调用GVK()方法设置GroupVersionKind")
	}

	// 检查 dest 类型
	switch stmt.Dest.(type) {
	case *string, *[]byte:
		// valid
	default:
		return fmt.Errorf("请确保 dest 是一个指向 string 或 []byte 的指针")
	}

	if namespaced {
		if stmt.AllNamespace {
			ns = metav1.NamespaceAll
		} else {
			if ns == "" {
				ns = metav1.NamespaceDefault
			}
		}
	} else {
		ns = metav1.NamespaceNone
	}

	showEvents := true
	if ctxVal := stmt.Context.Value("showEvents"); ctxVal != nil {
		if b, ok := ctxVal.(bool); ok {
			showEvents = b
		}
	}

	var output string
	var err error
	// 执行describe
	m := k.Status().DescriberMap()
	gk := schema.GroupKind{
		Group: gvk.Group,
		Kind:  gvk.Kind,
	}
	// 先从内置的describerMap中查找
	if d, ok := m[gk]; ok {
		output, err = d.Describe(ns, name, describe.DescriberSettings{
			ShowEvents: showEvents,
		})
		if err != nil {
			return fmt.Errorf("DescriberMap describe %s/%s error: %v", gvk.String(), name, err)
		}
	} else {
		// 没有内置描述器
		mapping := &meta.RESTMapping{
			Resource: k.Statement.GVR,
		}
		if gd, b := describe.GenericDescriberFor(mapping, k.RestConfig()); b {
			output, err = gd.Describe(ns, name, describe.DescriberSettings{
				ShowEvents: showEvents,
			})
			if err != nil {
				return fmt.Errorf("GenericDescriber describe %s/%s error: %v", gvk.String(), name, err)
			}
		}
	}

	// 将结果写入 tx.Statement.Dest
	switch dest := k.Statement.Dest.(type) {
	case *string:
		*dest = output
		klog.V(8).Infof("Describe result %s", *dest)
	case *[]byte:
		*dest = []byte(output)
		klog.V(8).Infof("Describe result %s", *dest)
	default:
		return fmt.Errorf("dest is neither *string nor *[]byte")
	}
	return nil
}
