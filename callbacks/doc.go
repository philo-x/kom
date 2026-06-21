package callbacks

import (
	"fmt"

	"github.com/weibaohui/kom/kom"
	"github.com/weibaohui/kom/kom/doc"
	"github.com/weibaohui/kom/utils"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

func Doc(k *kom.Kubectl) error {

	stmt := k.Statement
	gvk := k.Statement.GVK
	field := stmt.DocField

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

	cacheKey := fmt.Sprintf("%s/%s/%s/%s", gvk.Group, gvk.Version, gvk.Kind, field)
	result, err := utils.GetOrSetCache(stmt.ClusterCache(), cacheKey, stmt.CacheTTL, func() (result string, err error) {
		apiDoc := doc.DocField{
			Kind: gvk.Kind,
			ApiVersion: schema.GroupVersion{
				Group:   gvk.Group,
				Version: gvk.Version,
			},
			OpenapiSchema: k.Status().OpenAPISchema(),
		}
		result = apiDoc.GetApiDocV2(field)
		return
	})
	if err != nil {
		return err
	}

	// 将结果写入 tx.Statement.Dest
	switch dest := k.Statement.Dest.(type) {
	case *string:
		*dest = result
		klog.V(8).Infof("Doc result %s", *dest)
	case *[]byte:
		*dest = []byte(result)
		klog.V(8).Infof("Doc result %s", *dest)
	default:
		return fmt.Errorf("dest is neither *string nor *[]byte")
	}
	return nil
}
