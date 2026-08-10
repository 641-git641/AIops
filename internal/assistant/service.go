package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"aiops/internal/eventbus"
	"aiops/internal/incident"
	"aiops/internal/metrics"
	"aiops/internal/util"

	"gorm.io/gorm"
)

// service.go：会话 API 用例（AGENT_CHAT_PLAN §7 REST 端点）。
// 幂等：SendMessage 要求 Idempotency-Key——单事务「插消息 + 入队 run(ON CONFLICT)」，
// 冲突回滚并返回既有 run（防双击/断线重放产生重复消息与 run）。

// Service 会话服务。
type Service struct {
	db        *gorm.DB
	model     string // 生成模型名（run 记录）
	broker    *Broker
	audit     *eventbus.AuditService
	runner    *Runner // 仅用于指标（ActiveRuns）；run 执行在 runner worker
	incidents *incident.Service // 绑定对象校验（Phase 2：incident 上下文）
	m         *metrics.Registry // 反馈指标（可 nil）
}

// NewService 创建会话服务（incidents 用于绑定上下文校验，可 nil——不校验绑定对象存在性）。
func NewService(db *gorm.DB, model string, broker *Broker, audit *eventbus.AuditService, runner *Runner, incidents *incident.Service) *Service {
	return &Service{db: db, model: model, broker: broker, audit: audit, runner: runner, incidents: incidents}
}

// SetMetrics 注入指标注册表（可 nil——测试/未装配路径判空）。
func (s *Service) SetMetrics(m *metrics.Registry) { s.m = m }

// CreateThreadInput 新建会话输入（contextType/contextId 可选：绑定 incident 上下文，Phase 2）。
type CreateThreadInput struct {
	Title       string `json:"title"`
	ContextType string `json:"contextType"` // "" | incident（alert 绑定 Phase 3+）
	ContextID   string `json:"contextId"`
}

// ErrBadContext 绑定上下文非法（API 层映射 400）。
var ErrBadContext = errors.New("assistant: 非法绑定上下文")

// ErrIncidentNotFound 绑定的事故不存在（API 层映射 400）。
var ErrIncidentNotFound = errors.New("assistant: 绑定的事故不存在")

// CreateThread 新建会话（默认租户归属；标题缺省「新会话」/绑定事故名）。
// 绑定 incident 时校验存在性（防止绑定幽灵事故）。
func (s *Service) CreateThread(ctx context.Context, tenantID string, in CreateThreadInput) (*AssistantThread, error) {
	title := in.Title
	switch in.ContextType {
	case "":
		if in.ContextID != "" {
			return nil, ErrBadContext
		}
	case "incident":
		if in.ContextID == "" {
			return nil, ErrBadContext
		}
		if s.incidents != nil {
			inc, err := s.incidents.Get(ctx, tenantID, in.ContextID)
			// incident.Service.Get 对不存在返回 gorm.ErrRecordNotFound（非 nil,nil）
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrIncidentNotFound
			}
			if err != nil {
				return nil, err
			}
			if inc == nil {
				return nil, ErrIncidentNotFound
			}
			if title == "" {
				title = firstNonEmpty(inc.AIGeneratedName, inc.Name)
			}
		}
	default:
		return nil, ErrBadContext
	}
	if title == "" {
		title = "新会话"
	}
	th := &AssistantThread{
		ID: util.NewID(), TenantID: tenantID, Title: title,
		Status: ThreadActive, CreatedBy: "local-noauth",
		ContextType: in.ContextType, ContextID: in.ContextID,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := CreateThread(ctx, s.db, th); err != nil {
		return nil, err
	}
	if s.audit != nil {
		_, _ = s.audit.Record(ctx, eventbus.AuditLog{
			TenantID: tenantID, Actor: "local-noauth",
			Action: "assistant.thread.created", Target: th.ID,
		})
	}
	return th, nil
}

// ListThreads 会话列表。
func (s *Service) ListThreads(ctx context.Context, tenantID string) ([]AssistantThread, error) {
	return ListThreads(ctx, s.db, tenantID, 50)
}

// GetThread 会话详情（不存在返回 nil,nil）。
func (s *Service) GetThread(ctx context.Context, tenantID, threadID string) (*AssistantThread, error) {
	return GetThread(ctx, s.db, tenantID, threadID)
}

// ListThreadMessages 会话消息列表（升序；供前端历史渲染）。
func (s *Service) ListThreadMessages(ctx context.Context, tenantID, threadID string) ([]AssistantMessage, error) {
	th, err := GetThread(ctx, s.db, tenantID, threadID)
	if err != nil {
		return nil, err
	}
	if th == nil {
		return nil, ErrNotFound
	}
	return ListMessages(ctx, s.db, tenantID, threadID)
}

// DeleteThread 软删会话。
func (s *Service) DeleteThread(ctx context.Context, tenantID, threadID string) error {
	th, err := GetThread(ctx, s.db, tenantID, threadID)
	if err != nil {
		return err
	}
	if th == nil {
		return ErrNotFound
	}
	if err := SoftDeleteThread(ctx, s.db, tenantID, threadID); err != nil {
		return err
	}
	if s.audit != nil {
		_, _ = s.audit.Record(ctx, eventbus.AuditLog{
			TenantID: tenantID, Actor: "local-noauth",
			Action: "assistant.thread.deleted", Target: threadID,
		})
	}
	return nil
}

// ErrNotFound 目标不存在（API 层映射 404）。
var ErrNotFound = errors.New("assistant: not found")

// ErrIdempotencyKeyRequired 缺少幂等键。
var ErrIdempotencyKeyRequired = errors.New("assistant: Idempotency-Key 头必填（防重复提交）")

// SendMessageResult 发送消息结果（202 返回）。
type SendMessageResult struct {
	ThreadID  string `json:"threadId"`
	MessageID string `json:"messageId"`
	RunID     string `json:"runId"`
}

// SendMessage 发送用户消息并创建 run。
// 幂等契约：同 (tenant, idemKey) 重复调用返回既有 run（不产生重复消息/run）。
func (s *Service) SendMessage(ctx context.Context, tenantID, threadID, content, idemKey string) (*SendMessageResult, error) {
	if idemKey == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	th, err := GetThread(ctx, s.db, tenantID, threadID)
	if err != nil {
		return nil, err
	}
	if th == nil {
		return nil, ErrNotFound
	}
	if content == "" {
		return nil, errors.New("assistant: message content 不能为空")
	}

	var res SendMessageResult
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 消息序号（线程内）
		seq, err := NextMessageSequence(ctx, tx, tenantID, threadID)
		if err != nil {
			return err
		}
		msg := &AssistantMessage{
			ID: util.NewID(), TenantID: tenantID, ThreadID: threadID,
			Role: MsgUser, Content: content, ContentFormat: "text",
			Sequence: seq, Status: "complete", CreatedAt: time.Now().UTC(),
		}
		if err := CreateMessage(ctx, tx, msg); err != nil {
			return err
		}
		run := &AssistantRun{
			ID: util.NewID(), TenantID: tenantID, ThreadID: threadID,
			InputMessageID: msg.ID, Model: s.model, PromptVersion: "v1",
			IdempotencyKey: idemKey, CreatedAt: time.Now().UTC(),
		}
		runID, err := EnqueueRun(ctx, tx, run)
		if err != nil {
			return err
		}
		res = SendMessageResult{ThreadID: threadID, MessageID: msg.ID, RunID: runID}
		_ = TouchThread(ctx, tx, tenantID, threadID, "")
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.audit != nil {
		_, _ = s.audit.Record(ctx, eventbus.AuditLog{
			TenantID: tenantID, CorrelationID: res.RunID, Actor: "local-noauth",
			Action: "assistant.message.sent", Target: threadID,
			Metadata: jsonOf(map[string]any{"messageId": res.MessageID}),
		})
	}
	return &res, nil
}

// GetRun 查询 run（含 steps）。
func (s *Service) GetRun(ctx context.Context, tenantID, runID string) (*AssistantRun, []AssistantStep, error) {
	run, err := GetRun(ctx, s.db, tenantID, runID)
	if err != nil {
		return nil, nil, err
	}
	if run == nil {
		return nil, nil, ErrNotFound
	}
	steps, err := ListSteps(ctx, s.db, tenantID, runID)
	if err != nil {
		return nil, nil, err
	}
	return run, steps, nil
}

// CancelRun 取消 run（仅 queued/running 可取消；worker 每轮自查状态）。
func (s *Service) CancelRun(ctx context.Context, tenantID, runID string) error {
	run, err := GetRun(ctx, s.db, tenantID, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return ErrNotFound
	}
	if run.Status != RunQueued && run.Status != RunRunning {
		return fmt.Errorf("assistant: run 状态 %s 不可取消", run.Status)
	}
	if err := CancelRun(ctx, s.db, tenantID, runID); err != nil {
		return err
	}
	if s.broker != nil {
		_, _ = s.broker.Publish(ctx, runID, tenantID, EvRunCancelled, map[string]any{})
	}
	return nil
}

// ActiveRuns 当前执行中 run 数（metrics gauge 求值）。
func (s *Service) ActiveRuns() float64 {
	if s.runner != nil {
		return float64(s.runner.ActiveRuns())
	}
	return 0
}

// ErrBadFeedback 反馈参数非法。
var ErrBadFeedback = errors.New("assistant: rating 必须为 useful 或 not_useful")

// AddFeedback 对最终 assistant 消息提交反馈（rating 白名单；落库 + 审计 + 指标）。
// 仅允许对 assistant 角色的 complete 消息反馈（工具轮空内容消息跳过）。
func (s *Service) AddFeedback(ctx context.Context, tenantID, messageID, rating, note string) error {
	if rating != FeedbackUseful && rating != FeedbackNotUseful {
		return ErrBadFeedback
	}
	msg, err := GetMessage(ctx, s.db, tenantID, messageID)
	if err != nil {
		return err
	}
	if msg == nil {
		return ErrNotFound
	}
	if msg.Role != MsgAssistant || msg.Content == "" {
		return errors.New("assistant: 仅可对最终回答反馈")
	}
	fb := &AssistantFeedback{
		ID: util.NewID(), TenantID: tenantID, MessageID: messageID,
		RunID: msg.RunID, Rating: rating, Note: truncateStr(note, 1000),
		CreatedAt: time.Now().UTC(),
	}
	if err := CreateFeedback(ctx, s.db, fb); err != nil {
		return err
	}
	if s.audit != nil {
		_, _ = s.audit.Record(ctx, eventbus.AuditLog{
			TenantID: tenantID, CorrelationID: msg.RunID, Actor: "local-noauth",
			Action: "assistant.message.feedback", Target: messageID,
			Metadata: jsonOf(map[string]any{"rating": rating}),
		})
	}
	if s.m != nil {
		s.m.Inc(metricFeedbackTotal, rating)
	}
	return nil
}

func jsonOf(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
