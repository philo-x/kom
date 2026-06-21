package callbacks

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/weibaohui/kom/kom"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

func ExecuteCommand(k *kom.Kubectl) error {

	stmt := k.Statement
	ns := stmt.Namespace
	name := stmt.Name
	command := stmt.Command
	args := stmt.Args
	containerName := stmt.ContainerName
	ctx := stmt.Context

	// 如果只有一个容器，可以不设置
	// if stmt.ContainerName == "" {
	// 	return fmt.Errorf("请调用ContainerName()方法设置Pod容器名称")
	// }
	if stmt.Command == "" {
		return fmt.Errorf("请调用Command()方法设置命令")
	}

	// 检查 dest 类型
	switch stmt.Dest.(type) {
	case *string, *[]byte:
		// valid
	default:
		return fmt.Errorf("请确保 dest 是一个指向 string 或 []byte 的指针")
	}

	var err error
	cmd := []string{command}
	cmd = append(cmd, args...)
	klog.V(8).Infof("Execute %s %v in [%s/%s:%s]\n", command, args, ns, name, containerName)

	req := k.Client().CoreV1().RESTClient().
		Post().
		Namespace(ns).
		Resource("pods").
		Name(name).
		SubResource("exec").
		Param("container", containerName).
		Param("command", cmd[0])

	for _, arg := range cmd[1:] {
		req.Param("command", arg)
	}

	req.Param("tty", "false").
		Param("stdin", fmt.Sprintf("%v", stmt.Stdin != nil)).
		Param("stdout", "true").
		Param("stderr", "true")

	executor, err := createExecutor(req.URL(), k.RestConfig())
	if err != nil {
		return fmt.Errorf("error creating executor: %v", err)
	}

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer

	options := &remotecommand.StreamOptions{
		Stdout: &outBuf,
		Stderr: &errBuf,
		Tty:    false,
	}
	if stmt.Stdin != nil {
		options.Stdin = stmt.Stdin
	}
	err = executor.StreamWithContext(ctx, *options)
	if err != nil {
		s := errBuf.String()
		klog.V(8).Infof("Error executing command: %v", err)
		if strings.Contains(s, "Invalid argument") {
			return fmt.Errorf("系统参数错误 %v", s)
		}
		return fmt.Errorf("error executing command: %v %v", err, s)
	}

	// 将结果写入 tx.Statement.Dest
	switch dest := k.Statement.Dest.(type) {
	case *string:
		*dest = outBuf.String()
		klog.V(8).Infof("Execute result %s", *dest)
	case *[]byte:
		*dest = outBuf.Bytes()
		klog.V(8).Infof("Execute result %s", *dest)
	default:
		return fmt.Errorf("dest is neither *string nor *[]byte")
	}
	return nil
}

func createExecutor(url *url.URL, config *rest.Config) (remotecommand.Executor, error) {

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", url)
	if err != nil {
		return nil, err
	}
	// Fallback executor is default, unless feature flag is explicitly disabled.
	// WebSocketExecutor must be "GET" method as described in RFC 6455 Sec. 4.1 (page 17).
	websocketExec, err := remotecommand.NewWebSocketExecutor(config, "GET", url.String())
	if err != nil {
		return nil, err
	}
	exec, err = remotecommand.NewFallbackExecutor(websocketExec, exec, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return nil, err
	}
	return exec, nil
}
