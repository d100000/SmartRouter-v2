package service

import (
	"slices"
	"strconv"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
)

// SupplierTemplate is a safe, operator-installed connection capability. It
// never contains a plugin script, arbitrary headers, or upstream credentials.
type SupplierTemplate struct {
	ID            string   `json:"id"`
	Type          int      `json:"type"`
	Name          string   `json:"name"`
	Models        []string `json:"models"`
	Protocols     []string `json:"protocols"`
	TPMApplicable bool     `json:"tpm_applicable"`
}

func SupplierTemplates() []SupplierTemplate {
	result := []SupplierTemplate{}
	for _, kind := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeAnthropic, constant.ChannelTypeGemini, constant.ChannelTypeOpenRouter, constant.ChannelTypeMidjourney, constant.ChannelTypeMidjourneyPlus} {
		protocols := []string{"http"}
		tpm := true
		if kind == constant.ChannelTypeOpenAI {
			protocols = append(protocols, "realtime")
		}
		if kind == constant.ChannelTypeMidjourney || kind == constant.ChannelTypeMidjourneyPlus {
			protocols, tpm = []string{"async_task"}, false
		}
		result = append(result, SupplierTemplate{ID: strconv.Itoa(kind), Type: kind, Name: constant.GetChannelTypeName(kind), Models: []string{}, Protocols: protocols, TPMApplicable: tpm})
	}
	for _, plugin := range jsplugin.DefaultRegistry.Generation().Plugins() {
		tpm := false
		for _, name := range plugin.Meta.Models {
			units := FrozenTaskUsageUnits(plugin, name)
			input := units["input_tokens"] == "token" || units["prompt_tokens"] == "token"
			output := units["output_tokens"] == "token" || units["completion_tokens"] == "token"
			tpm = tpm || input && output
		}
		result = append(result, SupplierTemplate{ID: "plugin:" + plugin.Meta.Key, Type: constant.ChannelTypeTaskPlugin, Name: plugin.Meta.Name, Models: slices.Clone(plugin.Meta.Models), Protocols: []string{"async_task"}, TPMApplicable: tpm})
	}
	return result
}

func SupplierTemplateByID(id string) (SupplierTemplate, bool) {
	for _, template := range SupplierTemplates() {
		if template.ID == id {
			return template, true
		}
	}
	return SupplierTemplate{}, false
}

func SupplierChannelProtocols(channel *model.Channel) ([]string, bool) {
	if template, ok := SupplierTemplateByID(model.SupplierChannelTemplateID(channel)); ok {
		return template.Protocols, template.TPMApplicable
	}
	if channelUsesMeteredHTTP(channel.Type) {
		return []string{"http"}, true
	}
	return []string{}, false
}
