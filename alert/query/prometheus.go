package query

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"watchAlert/controllers/repo"
	"watchAlert/globals"
	models "watchAlert/models"
	"watchAlert/utils/prom"
)

type Prometheus struct {
	alertEvent models.AlertCurEvent
}

func (p *Prometheus) Query(rule models.AlertRule) {

	for _, dsId := range rule.DatasourceIdList {

		resQuery, _, err := prom.NewPromClient(dsId).Query(rule.RuleConfigJson.PromQL)
		if err != nil {
			return
		}

		if resQuery == nil {
			return
		}

		var curKeys []string

		for _, v := range resQuery {

			fingerprint := v.Labels.FastFingerprint().String()
			key := p.alertEvent.CurAlertCacheKey(rule.RuleId, fingerprint)
			curKeys = append(curKeys, key)

			// handle series tags
			metricMap := make(map[string]interface{})
			for label, value := range v.Labels {
				metricMap[string(label)] = string(value)
			}
			metricMap["value"] = v.Value

			metricArr := labelMapToArr(metricMap)
			sort.Strings(metricArr)

			event := models.AlertCurEvent{
				DatasourceType:       rule.DatasourceType,
				DatasourceIdList:     []string{dsId},
				Fingerprint:          fingerprint,
				RuleId:               rule.RuleId,
				RuleName:             rule.RuleName,
				Severity:             rule.RuleConfigJson.Severity,
				Instance:             string(v.Labels["instance"]),
				Metric:               strings.Join(metricArr, ",,"),
				MetricMap:            metricMap,
				PromQl:               rule.RuleConfigJson.PromQL,
				LabelsMap:            rule.LabelsMap,
				Labels:               rule.Labels,
				EvalInterval:         rule.EvalInterval,
				ForDuration:          rule.ForDuration,
				NoticeId:             rule.NoticeId,
				NoticeGroupList:      rule.NoticeGroupList,
				IsRecovered:          false,
				RepeatNoticeInterval: rule.RepeatNoticeInterval,
				DutyUser:             "暂无", // 默认暂无值班人员, 渲染模版时会实际判断 Notice 是否存在值班人员
			}
			event.Annotations = event.ParserAnnotation(rule.Annotations)
			event.FirstTriggerTime = event.GetFirstTime()
			event.LastEvalTime = event.GetLastEvalTime()
			event.LastSendTime = event.GetLastSendTime()

			p.alertEvent.SetCache(event, 0)
			err = repo.DBCli.Create(models.AlertCurEvent{}, &event)
			if err != nil {
				return
			}

		}

		allKey := p.alertCacheKeys(rule)

		recoverKeys := p.getSliceDifference(allKey, curKeys)

		for _, key := range recoverKeys {
			event := p.alertEvent.GetCache(key)
			if event.IsRecovered == true {
				continue
			}
			event.IsRecovered = true
			event.RecoverTime = time.Now().Unix()
			event.LastSendTime = 0
			p.alertEvent.SetCache(event, 0)
		}

	}

}

func (p *Prometheus) alertCacheKeys(rule models.AlertRule) []string {

	// 获取所有keys
	keyPrefix := p.alertEvent.CurAlertCacheKey(rule.RuleId, "*")
	keys, _ := globals.RedisCli.Keys(keyPrefix).Result()

	return keys

}

func (p *Prometheus) getSliceDifference(slice1 []string, slice2 []string) []string {
	difference := []string{}

	// 遍历缓存
	for _, item1 := range slice1 {
		found := false
		// 遍历当前key
		for _, item2 := range slice2 {
			if item1 == item2 {
				found = true
				break
			}
		}
		// 添加到差异切片中
		if !found {
			difference = append(difference, item1)
		}
	}

	return difference
}

func labelMapToArr(m map[string]interface{}) []string {
	numLabels := len(m)

	labelStrings := make([]string, 0, numLabels)
	for label, value := range m {
		labelStrings = append(labelStrings, fmt.Sprintf("%s=%s", label, value))
	}

	if numLabels > 1 {
		sort.Strings(labelStrings)
	}

	return labelStrings
}