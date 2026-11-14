package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// DecodeParametersStrict 将扩展参数严格解码为对应模块拥有的参数合同。
// 它拒绝未知字段、尾随 JSON 值和空参数，错误不会回显参数内容。
func DecodeParametersStrict[T any](parameters apiextensionsv1.JSON) (T, error) {
	var result T
	if len(bytes.TrimSpace(parameters.Raw)) == 0 {
		return result, fmt.Errorf("decode task extension parameters: parameters are required")
	}

	decoder := json.NewDecoder(bytes.NewReader(parameters.Raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("decode task extension parameters: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return result, fmt.Errorf("decode task extension parameters: multiple JSON values are not allowed")
	} else if err != io.EOF {
		return result, fmt.Errorf("decode task extension parameters: %w", err)
	}
	return result, nil
}

// DecodeKubernetesDaemonSetCarrierParameters strictly decodes the DaemonSet
// carrier's parameters and fills in MaxUnavailable. Unlike the replicated
// carriers, absent parameters are valid: every field has a default. The Job
// Controller calls it to reject a bad per-node Task before dispatch, and the
// Agent to render the DaemonSet.
func DecodeKubernetesDaemonSetCarrierParameters(parameters apiextensionsv1.JSON) (KubernetesDaemonSetCarrierParametersV1, error) {
	result := KubernetesDaemonSetCarrierParametersV1{}
	if len(bytes.TrimSpace(parameters.Raw)) != 0 {
		decoded, err := DecodeParametersStrict[KubernetesDaemonSetCarrierParametersV1](parameters)
		if err != nil {
			return result, err
		}
		result = decoded
	}
	if result.MaxUnavailable == nil {
		one := intstr.FromInt32(1)
		result.MaxUnavailable = &one
	}
	value, err := intstr.GetScaledValueFromIntOrPercent(result.MaxUnavailable, 100, true)
	if err != nil || value < 1 || (result.MaxUnavailable.Type == intstr.String && value > 100) {
		return result, fmt.Errorf("maxUnavailable must be a positive integer or a percentage from 1%% to 100%%")
	}
	return result, nil
}
