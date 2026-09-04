package feishu

import (
	"sort"
	"strconv"
	"strings"
)

const (
	askQuestionCheckerNamePrefix = "askq_option_"
	askQuestionSubmitNamePrefix  = "askq_multi_submit_"
)

func askQuestionCheckerName(value string) string {
	return askQuestionCheckerNamePrefix + value
}

func askQuestionSubmitName(action, cardID string) string {
	parts := strings.SplitN(action, ":", 4)
	questionIndex := "0"
	if len(parts) >= 3 && parts[0] == "askq" && parts[2] == "multi" {
		questionIndex = parts[1]
	}
	return askQuestionSubmitNamePrefix + cardID + "_" + questionIndex
}

func parseAskQuestionSubmitName(name string) (action, cardID string, ok bool) {
	if !strings.HasPrefix(name, askQuestionSubmitNamePrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(name, askQuestionSubmitNamePrefix)
	separator := strings.LastIndexByte(rest, '_')
	if separator <= 0 || separator+1 >= len(rest) {
		return "", "", false
	}
	cardID = rest[:separator]
	questionIndex := rest[separator+1:]
	index, err := strconv.Atoi(questionIndex)
	if err != nil || index < 0 {
		return "", "", false
	}
	return "askq:" + questionIndex + ":multi", cardID, true
}

func collectAskQuestionValuesFromForm(formValue map[string]any) []string {
	if len(formValue) == 0 {
		return nil
	}
	values := make([]int, 0, len(formValue))
	seen := make(map[int]struct{}, len(formValue))
	for key, value := range formValue {
		if !strings.HasPrefix(key, askQuestionCheckerNamePrefix) || !isTruthyFormValue(value) {
			continue
		}
		ordinal, err := strconv.Atoi(strings.TrimPrefix(key, askQuestionCheckerNamePrefix))
		if err != nil || ordinal < 1 {
			continue
		}
		if _, exists := seen[ordinal]; exists {
			continue
		}
		seen[ordinal] = struct{}{}
		values = append(values, ordinal)
	}
	sort.Ints(values)
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, strconv.Itoa(value))
	}
	return result
}
