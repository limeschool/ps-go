package engine

import (
	json2 "encoding/json"
	"fmt"
	json "github.com/json-iterator/go"
	"github.com/limeschool/gin"
	"github.com/robertkrimen/otto"
	"go.uber.org/zap"
	"ps-go/consts"
	"ps-go/errors"
	"ps-go/model"
	"ps-go/tools"
	"ps-go/tools/pool"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

type runtime struct {
	vm           *otto.Otto      // js 运行虚拟器
	wg           *sync.WaitGroup // 运行时锁，与runner共用一个锁
	component    Component       // 运行组件信息
	ctx          *gin.Context    // 上下文
	retry        int             // 重试次数
	maxRetry     int             // 最大重试次数
	retryMaxWait int             // 重试最大等待时长
	action       int             // 当前所在步数
	step         int             // 当前所在层级
	response     *responseChan   // 返回通道
	err          *errorChan      // 错误通道
	version      string          // 当前运行的版本
	trx          string          // 请求唯一标志

	runStore     RunStore     // 运行存储器
	store        Store        // 全局存储器
	stepLog      StepLog      //层级日志管理器
	componentLog ComponentLog // 组件日志管理器
}

func (r *runtime) setLog(resp any, err error, t time.Time) {
	r.componentLog.SetRunTime(t)
	// 设置请求数据
	r.componentLog.SetRequest(r.component)
	// 设置返回数据
	r.componentLog.SetResponse(resp)
	// 设置错误
	r.componentLog.SetError(err)
	// 设置重试此处
	r.componentLog.SetRetryCount(r.retry)
	// 设置当前步数
	r.componentLog.SetStep(r.step + 1)
	// 设置当前步数
	r.componentLog.SetAction(r.action + 1)
}

func (r *runtime) Run() {
	defer func() { // 防止意外Panic
		if p := recover(); p != nil {
			r.ctx.Log.Error("recover", zap.Any("panic", p))
		}
	}()

	var resp any
	var err error

	// 创建组件日志
	r.componentLog = r.stepLog.NewComponentLog(r.step, r.action)
	// 设置组件日志
	defer r.setLog(resp, err, time.Now())

	// 判断是否跳过
	entry, err := r.GetConditionResult(r.component.Condition, nil)
	if err != nil {
		r.err.SetAndClose(err, r.wg)
	}

	if !entry {
		r.componentLog.SetSkip(true)
		r.wg.Done()
		return
	}
	// 进行任务执行
	// 进行变量参数转换
	r.transferData()

	//判断是否使用缓存
	cache := r.newRunCache()
	if r.component.IsCache { //从缓存读取数据
		if resp, err = cache.getCache(); err == nil {
			r.componentLog.SetOutputData(resp)
			r.runStore.SetData(r.component.OutputName, resp)
			r.wg.Done()
			return
		}
	}

	if r.component.Type == ComponentTypeApi {
		resp, err = r.runApi()
	} else {
		resp, err = r.runScript()
	}

	//处理请求异常
	if err != nil && !r.component.IgnoreError {
		// 设置执行错误日志
		r.componentLog.SetError(err)

		if r.retry <= r.maxRetry && r.IsRetry(err) {
			if r.retryMaxWait != 0 {
				time.AfterFunc(r.getWaitTime(r.retry, r.maxRetry, r.retryMaxWait), func() {
					_ = pool.Get().Invoke(r)
				})
			} else {
				_ = pool.Get().Invoke(r)
			}
			r.retry++
		} else {
			r.err.SetAndClose(err, r.wg)
		}

		return
	}

	if r.component.OutputData != nil {
		resp = r.GetOutputData(r.component.OutputData, resp)
	}

	if r.component.OutputName != "" {
		r.runStore.SetData(r.component.OutputName, resp)
		r.componentLog.SetOutputData(resp)

		if r.component.IsCache {
			cache.setCache(resp)
		}
	}

	if r.component.NowResponse { //设置了立即
		r.response.SetAndClose(nil)
	}
	r.wg.Done()
}

func (r *runtime) newRunCache() *runCache {
	return &runCache{
		r,
	}
}

// runApi 请求url 接口
func (r *runtime) runApi() (any, error) {
	com := r.component

	var header = make(map[string]string)
	var auth = make([]string, 0)

	// 转换header
	if len(com.Header) != 0 {
		for key, val := range com.Header {
			header[key] = fmt.Sprint(val)
		}
	}

	// 转换auth
	if len(com.Auth) != 0 {
		for key, val := range com.Auth {
			auth[key] = fmt.Sprint(val)
		}
	}

	request := tools.HttpRequest{
		Url:          com.Url,
		Method:       com.Method,
		Header:       header,
		Auth:         auth,
		Body:         com.Input,
		ContentType:  com.ContentType,
		Timeout:      com.Timeout,
		ResponseType: com.ResponseType,
		RequestType:  com.RequestType,
		XmlName:      com.XmlName,
	}

	if com.Tls != nil {
		caSecret := model.Secret{}
		if err := caSecret.OneByName(r.ctx, com.Tls.Ca); err != nil {
			return nil, errors.NewF("tls.ca name found err :%v", err.Error())
		}
		keySecret := model.Secret{}
		if err := keySecret.OneByName(r.ctx, com.Tls.Key); err != nil {
			return nil, errors.NewF("tls.key name found err :%v", err.Error())
		}
		request.Tls = &tools.Tls{
			Ca:  []byte(caSecret.Context),
			Key: []byte(keySecret.Context),
		}
	}

	// 设置api的请求日志
	defer r.componentLog.SetApiRequest(request)

	resp, err := request.Result()
	if err != nil {
		return resp, err
	}

	data, ok := resp.(map[string]any)
	if !ok {
		return resp, nil
	}

	// 获取返回表达式
	is, err := r.GetConditionResult(r.component.ResponseCondition, resp)
	if err != nil {
		return nil, err
	}

	// 表达式为false
	if !is {
		reg := regexp.MustCompile(`\{(\w|\.)+}`)
		if str := reg.FindString(r.component.ErrorMsg); str != "" {
			return nil, errors.New(fmt.Sprint(tools.GetMapData(str[1:len(str)-1], data)))
		}
		return nil, errors.New(r.component.ErrorMsg)
	}

	return data, nil
}

func (r *runtime) GetOutputData(outputData any, data any) any {
	inData := map[string]any{}

	switch data.(type) {
	case map[string]any:
		inData = data.(map[string]any)
	case map[string]string:
		temp := data.(map[string]string)
		for key, val := range temp {
			inData[key] = val
		}
	}
	st := &runStore{data: inData}
	return st.GetMatchData(outputData)
}

func (r *runtime) runScript() (resp any, err error) {
	defer func() {
		if p := recover(); p != nil {
			if e, ok := p.(*Error); ok {
				err = e
			} else {
				err = NewSystemPanicError(fmt.Sprint(p))
			}
		}
	}()

	script, version, err := r.store.LoadScript(r.ctx, r.component.Url)
	if err != nil {
		return nil, err
	}

	r.version = version
	// 设置输出日志版本
	r.componentLog.SetVersion(version)

	r.vm = otto.New()
	go r.waitTimeout()

	if _, err = r.vm.Run(script); err != nil {
		return nil, NewRunScriptError(err.Error())
	}

	// 获取调用入参
	ctx := GetGlobalJsModule(r)
	input := r.component.Input

	// 调用执行
	value, err := r.vm.Call(consts.ProcessScheduleFunc, r.ctx, ctx, input)
	if err != nil {
		return nil, NewRunScriptFuncError(err.Error())
	}

	// 返回结果
	respData, err := value.Export()
	if err != nil {
		return nil, NewScriptFuncReturnError(err.Error())
	}

	return respData, nil
}

// transferData 对可输入变量字段进行转换
func (r *runtime) transferData() {
	if len(r.component.Header) != 0 {
		r.runStore.GetMatchData(r.component.Header)
	}

	if r.component.Input != nil {
		// input可能为字符类型，
		r.component.Input = r.runStore.GetMatchData(r.component.Input)
	}

	if len(r.component.Auth) != 0 {
		r.runStore.GetMatchData(r.component.Auth)
	}

}

// waitTimeout 监听等待超时
func (r *runtime) waitTimeout() {
	r.vm.Interrupt = make(chan func(), 1)

	if r.component.Timeout <= 0 || r.component.Timeout > consts.ComponentExecSecond {
		r.component.Timeout = consts.ComponentExecSecond
	}

	// 监听超时时间
	select {
	case <-time.After(time.Duration(r.component.Timeout) * time.Second):
		r.vm.Interrupt <- func() {
			panic(errors.NewF("run script %v timeout", r.component.Url))
		}
	}
}

// getWaitTime 计算下一次的重试时间
func (r *runtime) getWaitTime(cur, max, wait int) time.Duration {
	if wait == 0 {
		wait = 10
	}

	if max > 5 || max < 0 {
		max = 5
	}

	if cur >= max {
		return time.Duration(wait) * time.Second
	} else {
		return time.Duration((wait/max)*cur) * time.Second
	}
}

func (r *runtime) IsRetry(err error) bool {
	if e, ok := err.(*Error); ok {
		return e.Code == NetworkErrorCode
	}
	return false
}

// GetConditionResult 获取表达式结果
func (r *runtime) GetConditionResult(condition string, data any) (bool, error) {
	if strings.Trim(condition, " ") == "" {
		return true, nil
	}

	// 需要执行表达式
	reg := regexp.MustCompile(`\{(\w|\.)+}`)
	variable := reg.FindAllString(condition, -1)

	script := ""
	cond := condition

	for index, valIndex := range variable {
		key := fmt.Sprintf("a_%v", index)

		cond = strings.ReplaceAll(cond, valIndex, key)
		var newVal any
		if data == nil {
			newVal = r.runStore.GetData(valIndex[1 : len(valIndex)-1])
		} else {
			temp, _ := data.(map[string]any)
			newVal = tools.GetMapData(valIndex[1:len(valIndex)-1], temp)
		}
		if newVal == nil {
			script += fmt.Sprintf("var %v = null;", key)
			continue
		}

		// 进行变量转换
		switch newVal.(type) {
		case uint8, uint16, uint32, uint, uint64, int8, int16, int32, int, int64, float64, float32, bool:
			script += fmt.Sprintf("var %v = %v;", key, fmt.Sprint(newVal))
		case json.Number, json2.Number:
			script += fmt.Sprintf("var %v = %v;", key, fmt.Sprint(newVal))
		case string:
			script += fmt.Sprintf(`var %v = "%v";`, key, newVal.(string))

		case []any, map[string]any:
			str, _ := json.MarshalToString(newVal)
			script += fmt.Sprintf(`var %v = %v;`, key, str)

		default:
			tp := reflect.TypeOf(newVal)

			if tp.Kind() == reflect.Map || tp.Kind() == reflect.Slice {
				str, _ := json.MarshalToString(newVal)
				script += fmt.Sprintf(`var %v = %v;`, key, str)
			} else {
				//处理不了的数据值默认为 undefined
				script += fmt.Sprintf("var %v = undefined;", key)
			}
		}

	}

	vm := otto.New()
	script = fmt.Sprintf("function condition(){%v return %v;}", script, cond)
	_, err := vm.Run(script)
	if err != nil {
		return false, NewConditionError(err.Error())
	}

	condVal, err := vm.Call("condition", nil)
	if err != nil {
		return false, NewConditionError(err.Error())
	}
	if !condVal.IsBoolean() {
		return false, NewConditionError("准入表达式结果必须是bool")
	}
	return condVal.ToBoolean()
}
