// v2.go V2 API：知识空间、统一资源、当前身份。
//
// 路由挂在 /api/v2 下，与业务路由共用同一套网关中间件（认证 + 限流 + 校验）：
// 通过 ctx.Middleware 调用，身份由 principal.From(ctx) 提供，
// **不接受** 请求体里的 tenant_id / subject_id（V2 文档 14.6 明确禁止信任请求体身份）。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go_agent/internal/biz/agentfactory"
	"go_agent/internal/biz/resource"
	"go_agent/internal/biz/space"
	"go_agent/internal/biz/usage"
	"go_agent/internal/pkg/principal"
	"go_agent/internal/pkg/trace"

	kerrors "github.com/go-kratos/kratos/v3/errors"
	khttp "github.com/go-kratos/kratos/v3/transport/http"
)

// registerV2Routes 挂上 V2 资源与空间接口。
func registerV2Routes(srv *khttp.Server, spaceUC *space.UseCase, resUC *resource.UseCase, usageUC *usage.UseCase, taskUC *agentfactory.UseCase) {
	if srv == nil || spaceUC == nil || resUC == nil {
		return
	}
	r := srv.Route("/api/v2")

	// —— 当前身份 ——
	r.GET("/me", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			p := principal.From(c)
			return ctx.JSON(http.StatusOK, map[string]any{
				"issuer":       p.Issuer,
				"client_id":    p.ClientID,
				"subject_id":   p.SubjectID,
				"tenant_id":    p.TenantID,
				"roles":        p.Roles,
				"scopes":       p.Scopes,
				"departments":  p.DepartmentIDs,
				"auth_type":    p.AuthType,
				"identity_key": p.IdentityKey(),
				"is_admin":     p.Admin,
			})
		})
	})

	// —— 知识空间 ——
	r.GET("/spaces", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			list, err := spaceUC.List(c, principal.From(c))
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, map[string]any{"spaces": list, "total": len(list)})
		})
	})

	r.POST("/spaces", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			var in space.CreateInput
			if err := decodeJSON(ctx, &in); err != nil {
				return err
			}
			s, err := spaceUC.Create(c, principal.From(c), in)
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusCreated, s)
		})
	})

	r.GET("/spaces/detail", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			s, err := spaceUC.Get(c, principal.From(c), ctx.Query().Get("space_id"))
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, s)
		})
	})

	r.PATCH("/spaces", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			var in space.UpdateInput
			if err := decodeJSON(ctx, &in); err != nil {
				return err
			}
			s, err := spaceUC.Update(c, principal.From(c), ctx.Query().Get("space_id"), in)
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, s)
		})
	})

	r.DELETE("/spaces", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			if err := spaceUC.Delete(c, principal.From(c), ctx.Query().Get("space_id")); err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, map[string]any{"deleted": true})
		})
	})

	r.GET("/spaces/children", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			list, err := spaceUC.Children(c, principal.From(c), ctx.Query().Get("space_id"))
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, map[string]any{"spaces": list, "total": len(list)})
		})
	})

	r.GET("/spaces/shares", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			list, err := spaceUC.Shares(c, principal.From(c), ctx.Query().Get("space_id"))
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, map[string]any{"shares": list, "total": len(list)})
		})
	})

	r.POST("/spaces/shares", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			var in struct {
				PrincipalType string   `json:"principal_type"`
				PrincipalID   string   `json:"principal_id"`
				Actions       []string `json:"actions"`
			}
			if err := decodeJSON(ctx, &in); err != nil {
				return err
			}
			sh, err := spaceUC.Grant(c, principal.From(c), ctx.Query().Get("space_id"),
				in.PrincipalType, in.PrincipalID, in.Actions)
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusCreated, sh)
		})
	})

	r.DELETE("/spaces/shares", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			var id uint64
			parsed, err := strconv.ParseUint(strings.TrimSpace(ctx.Query().Get("share_id")), 10, 64)
			if err != nil {
				return kerrors.BadRequest("INVALID_ARGUMENT", "share_id 必须是数字")
			}
			id = parsed
			if err := spaceUC.Revoke(c, principal.From(c), id); err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, map[string]any{"deleted": true})
		})
	})

	// —— 统一资源 ——
	r.GET("/resources", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			f := resource.Filter{
				SpaceID:      strings.TrimSpace(ctx.Query().Get("space_id")),
				ResourceType: strings.TrimSpace(ctx.Query().Get("type")),
				OwnerID:      strings.TrimSpace(ctx.Query().Get("owner")),
			}
			list, err := resUC.List(c, principal.From(c), f)
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, map[string]any{"resources": list, "total": len(list)})
		})
	})

	r.POST("/resources", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			var in resource.Resource
			if err := decodeJSON(ctx, &in); err != nil {
				return err
			}
			got, err := resUC.Register(c, principal.From(c), &in)
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusCreated, got)
		})
	})

	r.GET("/resources/detail", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			got, err := resUC.Get(c, principal.From(c), ctx.Query().Get("resource_id"))
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, got)
		})
	})

	r.PATCH("/resources", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			var in resource.Resource
			if err := decodeJSON(ctx, &in); err != nil {
				return err
			}
			in.ResourceID = firstNonEmpty(in.ResourceID, ctx.Query().Get("resource_id"))
			got, err := resUC.Update(c, principal.From(c), &in)
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, got)
		})
	})

	r.DELETE("/resources", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			if err := resUC.Delete(c, principal.From(c), ctx.Query().Get("resource_id")); err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, map[string]any{"deleted": true})
		})
	})

	// 判权：POST /api/v2/resources/check {"resource_id","action"}
	r.POST("/resources/check", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			var in struct {
				ResourceID string `json:"resource_id"`
				Action     string `json:"action"`
			}
			if err := decodeJSON(ctx, &in); err != nil {
				return err
			}
			ok, err := resUC.Check(c, principal.From(c), in.ResourceID, in.Action)
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, map[string]any{"allowed": ok, "action": in.Action})
		})
	})

	r.GET("/resources/grants", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			list, err := resUC.Grants(c, principal.From(c), ctx.Query().Get("resource_id"))
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, map[string]any{"grants": list, "total": len(list)})
		})
	})

	r.POST("/resources/grants", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			var in struct {
				PrincipalType string   `json:"principal_type"`
				PrincipalID   string   `json:"principal_id"`
				Actions       []string `json:"actions"`
				Effect        string   `json:"effect"`
			}
			if err := decodeJSON(ctx, &in); err != nil {
				return err
			}
			if err := resUC.Grant(c, principal.From(c), ctx.Query().Get("resource_id"),
				in.PrincipalType, in.PrincipalID, in.Actions, in.Effect); err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusCreated, map[string]any{"ok": true})
		})
	})

	r.DELETE("/resources/grants", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			q := ctx.Query()
			if err := resUC.Revoke(c, principal.From(c), q.Get("resource_id"),
				q.Get("principal_type"), q.Get("principal_id"), q.Get("action")); err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, map[string]any{"deleted": true})
		})
	})

	r.GET("/resources/versions", func(ctx khttp.Context) error {
		return withMiddleware(ctx, func(c context.Context) error {
			list, err := resUC.Versions(c, principal.From(c), ctx.Query().Get("resource_id"))
			if err != nil {
				return v2Error(err)
			}
			return ctx.JSON(http.StatusOK, map[string]any{"versions": list, "total": len(list)})
		})
	})

	// —— 成本中心 ——
	if usageUC != nil {
		// POST /api/v2/usage/records：记录一次用量（worker / 网关 / 业务侧调用）
		r.POST("/usage/records", func(ctx khttp.Context) error {
			return withMiddleware(ctx, func(c context.Context) error {
				var in usage.Record
				if err := decodeJSON(ctx, &in); err != nil {
					return err
				}
				rec, err := usageUC.Record(c, principal.From(c), &in)
				if err != nil {
					return v2Error(err)
				}
				return ctx.JSON(http.StatusCreated, rec)
			})
		})

		// GET /api/v2/usage/summary?group_by=agent|model|kind|day|task|ref&since=RFC3339&until=RFC3339
		// 单任务：task_id；单次评测运行：ref_kind=harness_run&ref_id=<runID>（或 group_by=ref 纵横对比）
		r.GET("/usage/summary", func(ctx khttp.Context) error {
			return withMiddleware(ctx, func(c context.Context) error {
				q := ctx.Query()
				f := usage.Filter{
					AgentID:   strings.TrimSpace(q.Get("agent_id")),
					SubjectID: strings.TrimSpace(q.Get("subject_id")),
					TaskID:    strings.TrimSpace(q.Get("task_id")),
					RefKind:   strings.TrimSpace(q.Get("ref_kind")),
					RefID:     strings.TrimSpace(q.Get("ref_id")),
					GroupBy:   strings.TrimSpace(q.Get("group_by")),
				}
				// kind 只在显式指定时过滤：KindOf("") 会归一化成 llm，
				// 直接赋值等于把 embedding / mcp 等非 llm 成本永久排除在汇总之外
				if k := strings.TrimSpace(q.Get("kind")); k != "" {
					f.Kind = usage.KindOf(k)
				}
				var err error
				if f.Since, err = parseTime(q.Get("since")); err != nil {
					return kerrors.BadRequest("INVALID_ARGUMENT", "since 需为 RFC3339 时间")
				}
				if f.Until, err = parseTime(q.Get("until")); err != nil {
					return kerrors.BadRequest("INVALID_ARGUMENT", "until 需为 RFC3339 时间")
				}
				list, err := usageUC.Summary(c, principal.From(c), f)
				if err != nil {
					return v2Error(err)
				}
				return ctx.JSON(http.StatusOK, map[string]any{"buckets": list, "total": len(list)})
			})
		})
	}

	// —— 执行链路（V2-P2）：执行体 / 租约 / 死信 ——
	// 运维视角，只对管理员开放；认证未启用（AuthType=dev）时放行，避免本地开发被 403 卡住。
	if taskUC != nil {
		r.GET("/workers", func(ctx khttp.Context) error {
			return withMiddleware(ctx, func(c context.Context) error {
				if err := requireOperator(principal.From(c)); err != nil {
					return err
				}
				list, err := taskUC.ListWorkers(c)
				if err != nil {
					return v2Error(err)
				}
				online := 0
				for _, w := range list {
					if w.Alive && w.Status == agentfactory.WorkerOnline {
						online++
					}
				}
				return ctx.JSON(http.StatusOK, map[string]any{
					"workers": list,
					"total":   len(list),
					"online":  online,
				})
			})
		})

		// 运行中任务的租约视图：谁在跑、还剩多久到期——排查「卡在 running」的第一现场
		r.GET("/tasks/leases", func(ctx khttp.Context) error {
			return withMiddleware(ctx, func(c context.Context) error {
				p := principal.From(c)
				if err := requireOperator(p); err != nil {
					return err
				}
				limit := parseLimit(ctx.Query().Get("limit"), 50)
				tasks, err := taskUC.ListTasks(c, agentfactory.TaskFilter{
					Status:   agentfactory.TaskRunning,
					TenantID: p.TenantID,
					Limit:    limit,
				})
				if err != nil {
					return v2Error(err)
				}
				type leaseView struct {
					TaskID         uint64     `json:"task_id"`
					AgentKey       string     `json:"agent_key"`
					Title          string     `json:"title"`
					Attempt        int        `json:"attempt"`
					LeaseOwner     string     `json:"lease_owner"`
					LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
					DeadlineAt     *time.Time `json:"deadline_at,omitempty"`
					StartedAt      *time.Time `json:"started_at,omitempty"`
					TenantID       string     `json:"tenant_id"`
				}
				out := make([]leaseView, 0, len(tasks))
				for _, t := range tasks {
					v := leaseView{
						TaskID:     t.ID,
						AgentKey:   t.AgentKey,
						Title:      t.Title,
						Attempt:    t.Attempt,
						LeaseOwner: t.LeaseOwner,
						TenantID:   t.TenantID,
					}
					if !t.LeaseExpiresAt.IsZero() {
						ts := t.LeaseExpiresAt
						v.LeaseExpiresAt = &ts
					}
					if !t.DeadlineAt.IsZero() {
						ts := t.DeadlineAt
						v.DeadlineAt = &ts
					}
					if !t.StartedAt.IsZero() {
						ts := t.StartedAt
						v.StartedAt = &ts
					}
					out = append(out, v)
				}
				return ctx.JSON(http.StatusOK, map[string]any{"leases": out, "total": len(out)})
			})
		})

		// 死信：重试次数用尽仍失败的任务。人工确认原因后走 v1 的重试接口复活。
		r.GET("/tasks/dead-letters", func(ctx khttp.Context) error {
			return withMiddleware(ctx, func(c context.Context) error {
				p := principal.From(c)
				if err := requireOperator(p); err != nil {
					return err
				}
				list, err := taskUC.ListDeadLetters(c, p.TenantID, parseLimit(ctx.Query().Get("limit"), 50))
				if err != nil {
					return v2Error(err)
				}
				return ctx.JSON(http.StatusOK, map[string]any{"tasks": list, "total": len(list)})
			})
		})

		// 任务执行视图：v1 proto 还没有 snapshot_id / delegated_caps 字段，
		// 这里先提供只读查询（租户隔离），供审计与前端任务详情使用。
		r.GET("/tasks/detail", func(ctx khttp.Context) error {
			return withMiddleware(ctx, func(c context.Context) error {
				p := principal.From(c)
				id, err := parseUintQuery(ctx.Query().Get("id"))
				if err != nil {
					return kerrors.BadRequest("INVALID_ARGUMENT", "id 必须是正整数")
				}
				t, err := taskUC.GetTask(c, id)
				if err != nil {
					return v2Error(err)
				}
				if t.TenantID != "" && t.TenantID != p.TenantID && !p.Admin {
					return kerrors.NotFound("NOT_FOUND", "任务不存在")
				}
				return ctx.JSON(http.StatusOK, map[string]any{
					"task_id":        t.ID,
					"agent_key":      t.AgentKey,
					"status":         t.Status,
					"snapshot_id":    t.SnapshotID,
					"delegated_caps": t.DelegatedCaps,
					"depth":          t.Depth,
					"parent_task_id": t.ParentTaskID,
					"attempt":        t.Attempt,
					"trace_id":       t.TraceID,
				})
			})
		})

		// —— 链路追踪（P4）：一次请求一条 Trace，span 覆盖 task / llm / mcp / skill / rag ——
		//
		// 内存环形存储（最近 N 条），进程重启即丢；配了 OTEL_EXPORTER_OTLP_ENDPOINT
		// 时同一份数据也会送到 collector，长期留存要在那边查。
		// 权限：指定 task_id 时按任务归属校验（与 /tasks/detail 同口径）；
		// 不指定等于浏览全局链路，属运维视角。
		r.GET("/traces", func(ctx khttp.Context) error {
			return withMiddleware(ctx, func(c context.Context) error {
				p := principal.From(c)
				store := trace.StoreOf()
				if store == nil {
					// 追踪未初始化不是错误：老部署/单测环境下前端应优雅降级
					return ctx.JSON(http.StatusOK, map[string]any{
						"enabled": false, "traces": []any{}, "total": 0,
					})
				}
				q := ctx.Query()
				f := trace.SpanFilter{
					TaskID: strings.TrimSpace(q.Get("task_id")),
					Name:   strings.TrimSpace(q.Get("name")),
					Limit:  parseLimit(q.Get("limit"), 20),
				}
				if f.TaskID != "" {
					id, err := parseUintQuery(f.TaskID)
					if err != nil {
						return kerrors.BadRequest("INVALID_ARGUMENT", "task_id 必须是正整数")
					}
					t, terr := taskUC.GetTask(c, id)
					if terr != nil {
						return v2Error(terr)
					}
					if t.TenantID != "" && t.TenantID != p.TenantID && !p.Admin {
						return kerrors.NotFound("NOT_FOUND", "任务不存在")
					}
					// 统一成任务行上的 ID 形态（避免 "0042" 这类等价写法查不到）
					f.TaskID = strconv.FormatUint(t.ID, 10)
				} else if err := requireOperator(p); err != nil {
					return err
				}
				list := store.List(f)
				return ctx.JSON(http.StatusOK, map[string]any{
					"enabled": true,
					"traces":  list,
					"total":   len(list),
					"stats":   store.Stats(),
				})
			})
		})

		// 单条 trace：按 trace_id 精确取（从任务行拿 trace_id 直接跳转）
		r.GET("/traces/detail", func(ctx khttp.Context) error {
			return withMiddleware(ctx, func(c context.Context) error {
				p := principal.From(c)
				store := trace.StoreOf()
				if store == nil {
					return kerrors.NotFound("NOT_FOUND", "链路追踪未启用")
				}
				traceID := strings.TrimSpace(ctx.Query().Get("trace_id"))
				if traceID == "" {
					return kerrors.BadRequest("INVALID_ARGUMENT", "trace_id 必填")
				}
				tr, ok := store.Get(traceID)
				if !ok {
					return kerrors.NotFound("NOT_FOUND", "trace 不存在或已被环形缓冲淘汰")
				}
				// 有任务归属的 trace 要过租户校验；无 task_id 的（例如评测运行）走运维权限
				if tr.TaskID != "" {
					if id, err := parseUintQuery(tr.TaskID); err == nil {
						if t, terr := taskUC.GetTask(c, id); terr == nil && t.TenantID != "" &&
							t.TenantID != p.TenantID && !p.Admin {
							return kerrors.NotFound("NOT_FOUND", "trace 不存在")
						}
					}
				} else if err := requireOperator(p); err != nil {
					return err
				}
				return ctx.JSON(http.StatusOK, tr)
			})
		})

		// —— Agent 配置快照（P3）：审计「这条任务当时跑的是哪一版配置」 ——
		// 列表对所有已认证主体开放（属于可观测信息，不含密钥）；
		// 发布是新版本的生成动作，按运维权限收紧。
		r.GET("/agents/snapshots", func(ctx khttp.Context) error {
			return withMiddleware(ctx, func(c context.Context) error {
				agentID, err := parseUintQuery(ctx.Query().Get("agent_id"))
				if err != nil {
					return kerrors.BadRequest("INVALID_ARGUMENT", "agent_id 必须是正整数")
				}
				list, err := taskUC.Snapshots(c, agentID, parseLimit(ctx.Query().Get("limit"), 20))
				if err != nil {
					return v2Error(err)
				}
				return ctx.JSON(http.StatusOK, map[string]any{"snapshots": list, "total": len(list)})
			})
		})

		r.POST("/agents/snapshots", func(ctx khttp.Context) error {
			return withMiddleware(ctx, func(c context.Context) error {
				if err := requireOperator(principal.From(c)); err != nil {
					return err
				}
				var req struct {
					AgentID uint64 `json:"agent_id"`
				}
				if err := decodeJSON(ctx, &req); err != nil {
					return err
				}
				snap, err := taskUC.PublishSnapshot(c, req.AgentID)
				if err != nil {
					return v2Error(err)
				}
				return ctx.JSON(http.StatusOK, snap)
			})
		})
	}
}

// parseUintQuery 解析正整数查询参数（缺失或非法返回错误，由调用方给出可读提示）
func parseUintQuery(raw string) (uint64, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid uint query: %q", raw)
	}
	return n, nil
}

// requireOperator 运维视角接口（执行体/租约/死信）的准入判断：
// 管理员，或认证未启用（本地开发态 AuthType=dev）。
// 这些数据带主机名与实例拓扑，不适合对普通租户成员开放。
func requireOperator(p *principal.Principal) error {
	if p.Admin || p.AuthType == "dev" || p.AuthType == "" {
		return nil
	}
	return kerrors.Forbidden("FORBIDDEN", "需要管理员权限")
}

// parseLimit 解析分页参数（默认值兜底，上限 200）
func parseLimit(raw string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return def
	}
	if n > 200 {
		return 200
	}
	return n
}

// decodeJSON 读取并校验 JSON 请求体（限 1MB，避免超大 body 打满内存）。
func decodeJSON(ctx khttp.Context, dst any) error {
	if ctx.Request().Body == nil {
		return kerrors.BadRequest("INVALID_ARGUMENT", "请求体不能为空")
	}
	dec := json.NewDecoder(io.LimitReader(ctx.Request().Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return kerrors.BadRequest("INVALID_ARGUMENT", "请求体不是合法 JSON："+err.Error())
	}
	return nil
}

// parseTime 解析可选时间参数；空串表示不限制。
func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

// v2Error 领域错误 → HTTP 状态码（与 rag_service 的口径一致）。
func v2Error(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, space.ErrNotFound), errors.Is(err, resource.ErrNotFound),
		errors.Is(err, agentfactory.ErrDefNotFound), errors.Is(err, agentfactory.ErrTaskNotFound):
		return kerrors.NotFound("NOT_FOUND", err.Error())
	case errors.Is(err, space.ErrForbidden), errors.Is(err, resource.ErrForbidden),
		errors.Is(err, agentfactory.ErrRuntimeDenied), errors.Is(err, agentfactory.ErrDelegationDeny),
		errors.Is(err, agentfactory.ErrCapabilityDeny), errors.Is(err, agentfactory.ErrSandboxDeny):
		return kerrors.Forbidden("FORBIDDEN", err.Error())
	case errors.Is(err, space.ErrInvalid), errors.Is(err, resource.ErrInvalid),
		errors.Is(err, agentfactory.ErrInvalidDef), errors.Is(err, agentfactory.ErrInvalidTask):
		return kerrors.BadRequest("INVALID_ARGUMENT", err.Error())
	case errors.Is(err, space.ErrConflict), errors.Is(err, resource.ErrConflict):
		return kerrors.Conflict("CONFLICT", err.Error())
	case errors.Is(err, space.ErrDepth), errors.Is(err, space.ErrCycle):
		return kerrors.BadRequest("INVALID_ARGUMENT", err.Error())
	default:
		return err
	}
}
