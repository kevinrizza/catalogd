package serverutil

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type ConnectionDetailsEnsurer struct {
	Client           kubernetes.Interface
	ServiceName      string
	ServiceNamespace string
}

func (c ConnectionDetailsEnsurer) EnsureConnectionDetailsConfigMap(certFile string /*cert *tls.Certificate*/) error {
	cert, err := os.ReadFile(certFile)
	if err != nil {
		return fmt.Errorf("unable to read cert file: %v", err)
	}

	oldConfigMap, err := c.Client.CoreV1().ConfigMaps("olmv1-system").Get(context.TODO(), "catalogd-connection-details", metav1.GetOptions{})
	if k8sErrors.IsNotFound(err) {
		defaultConfigMap := defaultConfigMap(cert, c.ServiceName, c.ServiceNamespace)

		_, err = c.Client.CoreV1().ConfigMaps("olmv1-system").Create(context.TODO(), defaultConfigMap, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("unable to create config data for catalogd: %v", err)
		}

		return nil
	} else if err != nil {
		return fmt.Errorf("get connection details config data from catalogd: %v", err)
	}

	newConfigMap := oldConfigMap.DeepCopy()
	newConfigMap.BinaryData["ca"] = cert
	newConfigMap.Data["internalServiceName"] = c.ServiceName
	newConfigMap.Data["internalServiceNamespace"] = c.ServiceNamespace

	_, err = c.Client.CoreV1().ConfigMaps("olmv1-system").Update(context.TODO(), newConfigMap, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("write connection details config data from catalogd: %v", err)
	}

	return nil
}

func defaultConfigMap(cert []byte, serviceName, serviceNamespace string) *corev1.ConfigMap {
	defaultConfigMap := &corev1.ConfigMap{
		Data:       make(map[string]string),
		BinaryData: make(map[string][]byte),
		ObjectMeta: metav1.ObjectMeta{
			Name:      "catalogd-connection-details",
			Namespace: "olmv1-system",
		},
	}
	defaultConfigMap.BinaryData["ca"] = cert
	defaultConfigMap.Data["internalServiceName"] = serviceName
	defaultConfigMap.Data["internalServiceNamespace"] = serviceNamespace

	return defaultConfigMap
}
