import { useCallback, useEffect, useRef, useState } from 'react';
import { llm } from '../api.js';
import { Alert, EmptyState, Panel, Spinner, StatCard } from '../components/UI.jsx';
import {
  IconArrowRight,
  IconSpark,
  IconTrash,
  IconUpload,
  IconGraph,
  IconImage,
  IconVideo,
} from '../components/Icons.jsx';

// 媒体生成：文生图 / 图生图 / 视频生成。
// 数据源与模型来自 LLM 数据源配置（image_models / video_models 字段），
// 厂商协议在服务端适配（OpenAI 兼容 /images 族 + 三家视频异步任务协议）。
const TABS = [
  { key: 't2i', label: '文生图', icon: IconSpark },
  { key: 'i2i', label: '图生图', icon: IconImage },
  { key: 'video', label: '视频生成', icon: IconVideo },
];

const IMG_SIZES = ['1024x1024', '960x1280', '768x1024', '1280x720', ''];
const VIDEO_SIZES = ['', '1280x720', '720x1280', '1920x1080'];

// 进行中状态（轮询继续的条件）
const isPending = (status) => status === 'queued' || status === 'running';

export default function MediaGen() {
  const [tab, setTab] = useState('t2i');
  const [providers, setProviders] = useState([]);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');

  // 图片表单（t2i / i2i 共用，i2i 多参考图）
  const [imgForm, setImgForm] = useState({
    provider: '',
    model: '',
    prompt: '',
    size: '1024x1024',
    count: 1,
    images: [], // data URI 列表（仅 i2i）
  });
  const [imgResult, setImgResult] = useState(null); // 最近一次生成记录

  // 视频表单与任务
  const [vidForm, setVidForm] = useState({
    provider: '',
    model: '',
    prompt: '',
    image: '', // 首帧参考图（data URI 或 URL，可选）
    size: '1280x720',
    duration_seconds: 5,
  });
  const [videoJob, setVideoJob] = useState(null); // 进行中的任务记录
  const [videoDone, setVideoDone] = useState(null); // 最近完成的任务记录

  // 历史
  const [history, setHistory] = useState({ images: [], videos: [], imgTotal: 0, vidTotal: 0 });

  // 计时：图片生成 / 视频轮询期间显示真实耗时
  const [elapsed, setElapsed] = useState(0);
  const startedAt = useRef(0);
  useEffect(() => {
    const active = busy || (videoJob && isPending(videoJob.status));
    if (!active) return undefined;
    setElapsed(0);
    const t = setInterval(() => setElapsed(Math.round((Date.now() - startedAt.current) / 1000)), 1000);
    return () => clearInterval(t);
  }, [busy, videoJob?.id, videoJob?.status]);

  const imgProviders = providers.filter((p) => (p.image_models || []).length > 0 && p.enabled);
  const vidProviders = providers.filter((p) => (p.video_models || []).length > 0 && p.enabled);

  const loadHistory = useCallback(async () => {
    try {
      const [imgs, vids] = await Promise.all([
        llm.listMediaRecords({ kind: 'image', limit: 24 }),
        llm.listMediaRecords({ kind: 'video', limit: 12 }),
      ]);
      setHistory({
        images: imgs.images || [],
        videos: vids.videos || [],
        imgTotal: imgs.total ?? 0,
        vidTotal: vids.total ?? 0,
      });
    } catch (err) {
      // 历史失败不阻塞生成
      console.warn('[media] 历史加载失败', err.message);
    }
  }, []);

  useEffect(() => {
    (async () => {
      try {
        const list = await llm.listProviders({ all: false });
        const provs = list.providers || [];
        setProviders(provs);
        // 自动选中第一个可用数据源与首个模型
        const firstImg = provs.find((p) => (p.image_models || []).length > 0);
        const firstVid = provs.find((p) => (p.video_models || []).length > 0);
        if (firstImg) setImgForm((f) => ({ ...f, provider: firstImg.name, model: firstImg.image_models[0] }));
        if (firstVid) setVidForm((f) => ({ ...f, provider: firstVid.name, model: firstVid.video_models[0] }));
      } catch (err) {
        setError(`数据源加载失败：${err.message}`);
      }
      loadHistory();
    })();
  }, [loadHistory]);

  // —— 数据源 / 模型切换 ——

  const pickImageProvider = (name) => {
    const p = imgProviders.find((x) => x.name === name);
    setImgForm((f) => ({ ...f, provider: name, model: p?.image_models?.[0] || '' }));
  };
  const pickVideoProvider = (name) => {
    const p = vidProviders.find((x) => x.name === name);
    setVidForm((f) => ({ ...f, provider: name, model: p?.video_models?.[0] || '' }));
  };

  // —— 参考图上传（转 data URI，走 img2img 的 image 参数） ——

  const addRefImage = (files) => {
    const file = files?.[0];
    if (!file) return;
    if (!file.type.startsWith('image/')) {
      setError('参考图只支持图片文件');
      return;
    }
    if (file.size > 10 << 20) {
      setError('参考图不能超过 10MB');
      return;
    }
    const reader = new FileReader();
    reader.onload = () => {
      setError('');
      setImgForm((f) => ({ ...f, images: [...f.images, reader.result] }));
    };
    reader.readAsDataURL(file);
  };

  const addVidImage = (files) => {
    const file = files?.[0];
    if (!file) return;
    if (!file.type.startsWith('image/')) {
      setError('首帧图只支持图片文件');
      return;
    }
    if (file.size > 10 << 20) {
      setError('首帧图不能超过 10MB');
      return;
    }
    const reader = new FileReader();
    reader.onload = () => {
      setError('');
      setVidForm((f) => ({ ...f, image: reader.result }));
    };
    reader.readAsDataURL(file);
  };

  // —— 生成 ——

  const generateImages = async (mode) => {
    setBusy('img');
    setError('');
    setNotice('');
    setImgResult(null);
    startedAt.current = Date.now();
    try {
      const res = await llm.generateImages({
        provider: imgForm.provider,
        model: imgForm.model,
        mode,
        prompt: imgForm.prompt,
        size: imgForm.size || '',
        count: mode === 'img2img' ? 1 : Number(imgForm.count) || 1,
        ...(mode === 'img2img' ? { images: imgForm.images } : {}),
      });
      setImgResult(res.record);
      setNotice(mode === 'img2img' ? '图生图完成' : '生成完成');
      loadHistory();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const submitVideo = async () => {
    setBusy('vid');
    setError('');
    setNotice('');
    setVideoDone(null);
    startedAt.current = Date.now();
    try {
      const res = await llm.submitVideo({
        provider: vidForm.provider,
        model: vidForm.model,
        prompt: vidForm.prompt,
        ...(vidForm.image ? { image: vidForm.image } : {}),
        size: vidForm.size || '',
        duration_seconds: Number(vidForm.duration_seconds) || 0,
      });
      setVideoJob(res.record);
      setNotice('任务已提交，正在生成（页面会自动刷新状态）');
      loadHistory();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  // 视频任务轮询：queued/running 时每 4s 查一次，终态落画廊
  useEffect(() => {
    if (!videoJob || !isPending(videoJob.status)) return undefined;
    const t = setTimeout(async () => {
      try {
        const res = await llm.getVideo({ id: videoJob.id });
        const rec = res.record;
        setVideoJob(isPending(rec.status) ? rec : null);
        if (!isPending(rec.status)) {
          setVideoDone(rec);
          loadHistory();
          if (rec.status !== 'succeeded') setError(`视频任务失败：${rec.error || '未知原因'}`);
        }
      } catch (err) {
        // 单次轮询失败不打断：下一轮再试
        console.warn('[media] 轮询失败', err.message);
      }
    }, 4000);
    return () => clearTimeout(t);
  }, [videoJob, loadHistory]);

  // —— 渲染 ——

  const imgFormPanel = (
    <Panel title="生成参数" desc="数据源与模型来自「模型数据源」里配置的图像生成模型">
      <div className="form">
        <div className="query-row">
          <label className="field inline">
            <span className="field-label">数据源</span>
            <select className="input" value={imgForm.provider} onChange={(e) => pickImageProvider(e.target.value)}>
              {imgProviders.length === 0 && <option value="">（无可用数据源，请先在模型页配置 image_models）</option>}
              {imgProviders.map((p) => (
                <option key={p.name} value={p.name}>
                  {p.name}
                </option>
              ))}
            </select>
          </label>
          <label className="field inline">
            <span className="field-label">模型</span>
            <select className="input" value={imgForm.model} onChange={(e) => setImgForm({ ...imgForm, model: e.target.value })}>
              {(imgProviders.find((p) => p.name === imgForm.provider)?.image_models || []).map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          </label>
        </div>
        <div className="query-row">
          <label className="field inline">
            <span className="field-label">分辨率</span>
            <select className="input" value={imgForm.size} onChange={(e) => setImgForm({ ...imgForm, size: e.target.value })}>
              {IMG_SIZES.map((s) => (
                <option key={s || 'default'} value={s}>
                  {s || '厂商默认'}
                </option>
              ))}
            </select>
          </label>
          {tab === 't2i' && (
            <label className="field inline">
              <span className="field-label">生成张数</span>
              <select className="input" value={imgForm.count} onChange={(e) => setImgForm({ ...imgForm, count: Number(e.target.value) })}>
                {[1, 2, 3, 4].map((n) => (
                  <option key={n} value={n}>
                    {n} 张
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
        {tab === 'i2i' && (
          <div className="field">
            <span className="field-label">参考图（1-4 张）</span>
            <div className="ref-grid">
              {imgForm.images.map((dataURI, i) => (
                <div key={i} className="ref-item">
                  <img src={dataURI} alt={`参考图 ${i + 1}`} />
                  <button
                    className="icon-btn"
                    type="button"
                    aria-label="移除"
                    onClick={() => setImgForm((f) => ({ ...f, images: f.images.filter((_, j) => j !== i) }))}
                  >
                    <IconTrash size={13} />
                  </button>
                </div>
              ))}
              {imgForm.images.length < 4 && (
                <label className="ref-add">
                  <IconUpload size={18} />
                  <span>上传图片</span>
                  <input type="file" accept="image/*" hidden onChange={(e) => addRefImage(e.target.files)} />
                </label>
              )}
            </div>
            <span className="cell-sub">图生图走厂商的 image 参考参数（OpenAI 数据源走 multipart /images/edits）</span>
          </div>
        )}
        <div className="field">
          <span className="field-label">提示词{tab === 'i2i' ? '（描述要把参考图改成什么样）' : '（越具体越好：主体 / 风格 / 构图 / 光线）'}</span>
          <textarea
            className="textarea"
            rows={4}
            placeholder={tab === 'i2i' ? '例如：改成赛博朋克风格，霓虹灯，雨夜街道' : '例如：雪山脚下的木屋，黄昏，油画风格，暖色调'}
            value={imgForm.prompt}
            onChange={(e) => setImgForm({ ...imgForm, prompt: e.target.value })}
          />
        </div>
        <div className="form-actions">
          <button
            className="btn primary"
            type="button"
            disabled={busy === 'img' || !imgForm.provider || !imgForm.prompt.trim()}
            onClick={() => generateImages(tab === 'i2i' ? 'img2img' : 'text2img')}
          >
            {busy === 'img' ? <Spinner text="生成中" /> : <IconSpark size={15} />}
            {busy === 'img' ? `生成中 ${elapsed}s` : tab === 'i2i' ? '开始图生图' : '开始生成'}
          </button>
        </div>
      </div>
    </Panel>
  );

  const vidFormPanel = (
    <Panel title="视频参数" desc="视频为异步任务：提交后页面自动轮询，也可以关页回来再看">
      <div className="form">
        <div className="query-row">
          <label className="field inline">
            <span className="field-label">数据源</span>
            <select className="input" value={vidForm.provider} onChange={(e) => pickVideoProvider(e.target.value)}>
              {vidProviders.length === 0 && <option value="">（无可用数据源，请先在模型页配置 video_models）</option>}
              {vidProviders.map((p) => (
                <option key={p.name} value={p.name}>
                  {p.name}
                </option>
              ))}
            </select>
          </label>
          <label className="field inline">
            <span className="field-label">模型</span>
            <select className="input" value={vidForm.model} onChange={(e) => setVidForm({ ...vidForm, model: e.target.value })}>
              {(vidProviders.find((p) => p.name === vidForm.provider)?.video_models || []).map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          </label>
        </div>
        <div className="query-row">
          <label className="field inline">
            <span className="field-label">分辨率</span>
            <select className="input" value={vidForm.size} onChange={(e) => setVidForm({ ...vidForm, size: e.target.value })}>
              {VIDEO_SIZES.map((s) => (
                <option key={s || 'default'} value={s}>
                  {s || '厂商默认'}
                </option>
              ))}
            </select>
          </label>
          <label className="field inline">
            <span className="field-label">时长（秒，0 = 厂商默认）</span>
            <input
              className="input"
              type="number"
              min={0}
              max={60}
              value={vidForm.duration_seconds}
              onChange={(e) => setVidForm({ ...vidForm, duration_seconds: Number(e.target.value) })}
            />
          </label>
        </div>
        <div className="field">
          <span className="field-label">首帧参考图（选填，图生视频）</span>
          {vidForm.image ? (
            <div className="ref-grid">
              <div className="ref-item">
                <img src={vidForm.image} alt="首帧" />
                <button className="icon-btn" type="button" aria-label="移除" onClick={() => setVidForm((f) => ({ ...f, image: '' }))}>
                  <IconTrash size={13} />
                </button>
              </div>
            </div>
          ) : (
            <div className="ref-grid">
              <label className="ref-add">
                <IconUpload size={18} />
                <span>上传首帧图（纯文生视频可跳过）</span>
                <input type="file" accept="image/*" hidden onChange={(e) => addVidImage(e.target.files)} />
              </label>
            </div>
          )}
        </div>
        <div className="field">
          <span className="field-label">提示词（画面描述与运镜）</span>
          <textarea
            className="textarea"
            rows={4}
            placeholder="例如：一只柯基在草地上奔跑，镜头缓缓跟随，日落逆光"
            value={vidForm.prompt}
            onChange={(e) => setVidForm({ ...vidForm, prompt: e.target.value })}
          />
        </div>
        <div className="form-actions">
          <button
            className="btn primary"
            type="button"
            disabled={busy === 'vid' || (videoJob && isPending(videoJob.status)) || !vidForm.provider || !vidForm.prompt.trim()}
            onClick={submitVideo}
          >
            {busy === 'vid' || (videoJob && isPending(videoJob.status)) ? <Spinner text="提交" /> : <IconGraph size={15} />}
            {videoJob && isPending(videoJob.status) ? `生成中 ${elapsed}s` : '提交生成任务'}
          </button>
        </div>
      </div>
    </Panel>
  );

  const imgResultPanel =
    imgResult &&
    (imgResult.ok ? (
      <Panel title="本次生成" desc={`${imgResult.provider} / ${imgResult.model} · ${imgResult.duration_ms}ms`}>
        <div className="gen-grid">
          {imgResult.outputs.map((url) => (
            <a key={url} href={url} target="_blank" rel="noopener noreferrer" className="gen-item">
              <img src={url} alt="生成结果" loading="lazy" />
            </a>
          ))}
        </div>
      </Panel>
    ) : (
      <Alert type="error">生成失败：{imgResult.error}</Alert>
    ));

  const videoJobPanel =
    videoJob &&
    isPending(videoJob.status) && (
      <Panel title="视频任务进行中" desc={`${videoJob.provider} / ${videoJob.model} · 已提交 ${elapsed}s`}>
        <div className="ingest-live">
          <div className="ingest-live-head">
            <strong>任务 #{videoJob.id}</strong>
            <span className="cell-sub">排队 / 生成一般需要 1-5 分钟</span>
          </div>
          <div className="pipe">
            <span className="pipe-node"><span className="pipe-index">1</span> 提交厂商</span>
            <span className="pipe-node"><span className="pipe-index">2</span> 排队 / 渲染</span>
            <span className="pipe-node"><span className="pipe-index">3</span> 下载成品</span>
          </div>
          <Alert type="info">状态 {videoJob.status}，每 4 秒自动刷新，可关闭页面稍后回来查看历史。</Alert>
        </div>
      </Panel>
    );

  return (
    <div className="stack">
      <div className="stat-grid">
        <StatCard label="图像数据源" value={imgProviders.length} hint="配置了 image_models" tone="blue" />
        <StatCard label="视频数据源" value={vidProviders.length} hint="配置了 video_models" tone="teal" />
        <StatCard label="累计图片" value={history.imgTotal} hint="image_generations 记录" tone="violet" />
        <StatCard label="累计视频任务" value={history.vidTotal} hint="video_generations 记录" tone="indigo" />
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}

      <div className="kb-bar">
        <div className="kb-tabs" role="tablist" aria-label="媒体生成分区">
          {TABS.map((t) => (
            <button key={t.key} type="button" role="tab" aria-selected={tab === t.key} className={`kb-tab${tab === t.key ? ' active' : ''}`} onClick={() => setTab(t.key)}>
              <t.icon size={15} />
              {t.label}
            </button>
          ))}
        </div>
        <span className="cell-sub">产物落本地 /api/files/，记录可在下方历史里回看</span>
      </div>

      {tab === 'video' ? (
        <div className="two-col">
          {vidFormPanel}
          <div className="stack">
            {videoJobPanel}
            {videoDone && (
              <Panel title="最近任务" desc={`${videoDone.provider} / ${videoDone.model} · ${videoDone.duration_ms}ms`}>
                {videoDone.status === 'succeeded' ? (
                  <video src={videoDone.output} controls preload="metadata" className="gen-video" />
                ) : (
                  <Alert type="error">任务失败：{videoDone.error}</Alert>
                )}
              </Panel>
            )}
          </div>
        </div>
      ) : (
        <div className="two-col">
          {imgFormPanel}
          <div className="stack">{imgResultPanel}</div>
        </div>
      )}

      <Panel title="图片历史" desc="最近 24 张（点击放大）">
        {history.images.length === 0 ? (
          <EmptyState icon={IconImage} title="还没有生成记录" desc={imgProviders.length ? '在上方提交一次生成' : '先到「模型数据源」给数据源配置图像生成模型'} />
        ) : (
          <div className="gen-grid">
            {history.images.flatMap((rec) =>
              (rec.outputs || []).map((url) => (
                <a key={`${rec.id}-${url}`} href={url} target="_blank" rel="noopener noreferrer" className="gen-item" title={rec.prompt}>
                  <img src={url} alt={rec.prompt?.slice(0, 20) || '生成图'} loading="lazy" />
                  <span className="gen-caption">{rec.prompt?.slice(0, 40)}</span>
                </a>
              )),
            )}
          </div>
        )}
      </Panel>

      <Panel title="视频历史" desc="最近 12 个任务（进行中的任务提交后自动刷新）">
        {history.videos.length === 0 ? (
          <EmptyState icon={IconVideo} title="还没有视频任务" desc={vidProviders.length ? '在上方提交一个生成任务' : '先到「模型数据源」给数据源配置视频生成模型'} />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>#</th>
                  <th>提示词</th>
                  <th>模型</th>
                  <th>状态</th>
                  <th>产物</th>
                  <th>提交时间</th>
                </tr>
              </thead>
              <tbody>
                {history.videos.map((rec) => (
                  <tr key={rec.id}>
                    <td>{rec.id}</td>
                    <td style={{ maxWidth: 320 }} className="cell-sub">
                      {rec.prompt?.slice(0, 60)}
                    </td>
                    <td>
                      {rec.provider} / {rec.model}
                    </td>
                    <td>
                      <StatusBadge status={rec.status} />
                    </td>
                    <td>
                      {rec.status === 'succeeded' && rec.output ? (
                        <a href={rec.output} target="_blank" rel="noopener noreferrer" className="btn ghost sm">
                          <IconArrowRight size={13} /> 播放
                        </a>
                      ) : (
                        <span className="cell-sub">{rec.error || '—'}</span>
                      )}
                    </td>
                    <td className="cell-sub">{rec.created_at?.replace('T', ' ').slice(0, 19)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
    </div>
  );
}

// StatusBadge 任务状态徽标
function StatusBadge({ status }) {
  const map = {
    queued: { text: '排队中', tone: 'warn' },
    running: { text: '生成中', tone: 'indigo' },
    succeeded: { text: '完成', tone: 'ok' },
    failed: { text: '失败', tone: 'default' },
  };
  const s = map[status] || { text: status || '—', tone: 'default' };
  return (
    <Badge tone={s.tone} style={s.text === '失败' ? { color: 'var(--danger)', borderColor: '#f3c6c6' } : undefined}>
      {s.text}
    </Badge>
  );
}
