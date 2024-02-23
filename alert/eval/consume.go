package eval

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"watchAlert/alert/notice"
	"watchAlert/controllers/repo"
	"watchAlert/controllers/services"
	"watchAlert/globals"
	"watchAlert/models"
)

type EvalConsume struct {
	sync.RWMutex
	models.AlertCurEvent
	RedisChannel string
	alertGroups  map[string][]models.AlertCurEvent
	num          int64
}

type InterEvalConsume interface {
	Run()
}

func NewInterEvalConsumeWork() InterEvalConsume {

	return &EvalConsume{
		alertGroups: make(map[string][]models.AlertCurEvent),
	}

}

// Run 启动告警消费进程
func (ec *EvalConsume) Run() {

	groupInterval := 10

	action := func() {
		alertsCurEventKeys := ec.getRedisKeys()
		for _, key := range alertsCurEventKeys {
			alert := ec.GetCache(key)
			fmt.Println(alert)
			if alert.Fingerprint == "" {
				return
			}

			ec.Lock()
			ec.alertGroups[alert.RuleId] = append(ec.alertGroups[alert.RuleId], alert)
			ec.Unlock()

			if len(ec.alertGroups[alert.RuleId]) > 0 {
				// 如果信号量满了就推送告警，并且初始化信号量
				if ec.num == int64(groupInterval) {
					curEvent := ec.filterAlerts(ec.alertGroups)
					ec.fireAlertEvent(curEvent)
					// 执行一波后 必须重新清空alertGroups组中的数据。
					ec.clear()
				}
				ec.num++
			}
		}
	}

	ticker := time.Tick(time.Second)

	go func() {
		for range ticker {
			action()
		}
	}()

}

func (ec *EvalConsume) clear() {

	ec.alertGroups = map[string][]models.AlertCurEvent{}
	ec.num = 0
}

// 获取缓存所有Keys
func (ec *EvalConsume) getRedisKeys() []string {
	var keys []string
	cursor := uint64(0)
	pattern := models.CachePrefix + "*"
	// 每次获取的键数量
	count := int64(100)

	for {
		var curKeys []string
		var err error

		curKeys, cursor, err = globals.RedisCli.Scan(cursor, pattern, count).Result()
		if err != nil {
			break
		}

		keys = append(keys, curKeys...)

		if cursor == 0 {
			break
		}
	}

	return keys
}

// 过滤重复性告警
func (ec *EvalConsume) filterAlerts(alertGroups map[string][]models.AlertCurEvent) map[string][]models.AlertCurEvent {

	var newAlertGroups = make(map[string][]models.AlertCurEvent)

	for _, alerts := range alertGroups {
		// 根据相同指纹进行去重
		newAlert := ec.removeDuplicates(alerts)
		// 将通过指纹去重后以Fingerprint为Key的Map转换成以原来RuleName为Key的Map (同一告警类型聚合)
		for _, alert := range newAlert {
			newAlertGroups[alert.RuleName] = append(newAlertGroups[alert.RuleName], alert)
		}
	}

	return newAlertGroups

}

// 指纹去重
func (ec *EvalConsume) removeDuplicates(alerts []models.AlertCurEvent) []models.AlertCurEvent {
	/*
		alert中有不重复字段，last_eval_time。
	*/

	latestAlert := make(map[string]models.AlertCurEvent)
	var newAlerts []models.AlertCurEvent

	for _, alert := range alerts {
		// 以最新为准
		latestAlert[alert.Fingerprint] = alert
	}

	for _, alert := range latestAlert {
		newAlerts = append(newAlerts, alert)
	}

	return newAlerts
}

func (ec *EvalConsume) fireAlertEvent(alertGroups map[string][]models.AlertCurEvent) {

	fireAlertsMap := make(map[string][]models.AlertCurEvent)
	recoverAlertsMap := make(map[string][]models.AlertCurEvent)

	var (
		syncLock sync.Mutex
		wg       sync.WaitGroup
	)

	for _, alerts := range alertGroups {
		for _, alert := range alerts {
			wg.Add(1)
			go func(alert models.AlertCurEvent) {
				defer wg.Done()
				if alert.IsRecovered {
					syncLock.Lock()
					recoverAlertsMap[alert.RuleName] = append(recoverAlertsMap[alert.RuleName], alert)
					syncLock.Unlock()

					ec.DelCache(ec.CurAlertCacheKey(alert.RuleId, alert.Fingerprint))
					// 记录历史告警
					err := ec.RecordAlertHisEvent(alert)
					if err != nil {
						return
					}
				} else if !alert.IsRecovered {
					// 持续时间
					if alert.LastEvalTime-alert.FirstTriggerTime < alert.ForDuration {
						return
					}
					// 判断告警是否符合触发条件
					if alert.LastSendTime == 0 || alert.LastEvalTime >= alert.LastSendTime+alert.RepeatNoticeInterval*60 {
						syncLock.Lock()
						fireAlertsMap[alert.RuleName] = append(fireAlertsMap[alert.RuleName], alert)
						syncLock.Unlock()
					}
				}

			}(alert)
		}
	}

	wg.Wait()

	for key, _ := range fireAlertsMap {
		ec.handleAlert(fireAlertsMap[key])
	}

	for key, _ := range recoverAlertsMap {
		ec.handleAlert(recoverAlertsMap[key])
	}

}

func (ec *EvalConsume) handleAlert(alerts []models.AlertCurEvent) {

	if alerts == nil {
		return
	}

	var (
		content  string
		alertOne models.AlertCurEvent
		curTime  = time.Now().Unix()
	)

	if len(alerts) > 1 {
		content = fmt.Sprintf("聚合 %d 条告警", len(alerts))
	}

	var wg sync.WaitGroup
	for _, alert := range alerts {
		wg.Add(1)
		go func(alert models.AlertCurEvent) {
			defer wg.Done()
			if !alert.IsRecovered {
				alert.LastSendTime = curTime
				alert.SetCache(alert, 0)
			}
		}(alert)
	}
	wg.Wait()

	// 聚合
	alertOne = alerts[0]
	alertOne.Annotations += "\n" + content

	noticeId := ec.noticeSplitGroup(alertOne)

	noticeData := services.NewInterAlertNoticeService().GetNoticeObject(noticeId)

	switch alertOne.DatasourceType {
	case "Prometheus":
		prom := &notice.Prometheus{}
		notice.NewEntryNotice(prom, alertOne, noticeData)
	}

}

// 告警分组
func (ec *EvalConsume) noticeSplitGroup(alert models.AlertCurEvent) string {

	if len(alert.NoticeGroupList) != 0 {
		var noticeGroup []map[string]string
		for _, v := range alert.NoticeGroupList {
			noticeGroup = append(noticeGroup, map[string]string{
				v["key"]:   v["value"],
				"noticeId": v["noticeId"],
			})
		}

		// 从Metric中获取Key/Value
		for metricKey, metricValue := range alert.MetricMap {
			// 如果配置分组的Key/Value 和 Metric中的Key/Value 一致，则使用分组的 noticeId，匹配不到则用默认的。
			for _, noticeInfo := range noticeGroup {
				value, ok := noticeInfo[metricKey]
				if ok && metricValue == value {
					noticeId := noticeInfo["noticeId"]
					return noticeId
				}
			}
		}
	}

	return alert.NoticeId

}

// RecordAlertHisEvent 记录历史告警
func (ec *EvalConsume) RecordAlertHisEvent(alert models.AlertCurEvent) error {

	metric, _ := json.Marshal(alert.MetricMap)
	hisData := models.AlertHisEvent{
		DatasourceId:     alert.DatasourceIdList[0],
		Fingerprint:      alert.Fingerprint,
		RuleId:           alert.RuleId,
		RuleName:         alert.RuleName,
		Severity:         alert.Severity,
		PromQl:           alert.PromQl,
		Metric:           string(metric),
		EvalInterval:     alert.EvalInterval,
		Annotations:      alert.Annotations,
		IsRecovered:      true,
		FirstTriggerTime: alert.FirstTriggerTime,
		LastEvalTime:     alert.LastEvalTime,
		LastSendTime:     alert.LastSendTime,
		RecoverTime:      alert.RecoverTime,
	}

	err := repo.DBCli.Create(models.AlertHisEvent{}, &hisData)
	if err != nil {
		return fmt.Errorf("RecordAlertHisEvent -> %s", err)
	}

	return nil

}