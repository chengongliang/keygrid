import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { CloudDownload, DollarSign, Pencil, Plus, Search, Trash2, TriangleAlert } from 'lucide-react'
import { api, type ModelPrice, type ModelPriceUpsert, type PriceSyncPreviewItem } from '@/lib/api'
import { Pagination } from '@/components/usage-charts'
import { Modal } from './Providers'

// AdminPrices 计费 P1：admin 全局模型价格表管理。
// 匹配规则：精确模型名 > "*" 兜底行 > 未定价（cost=0，免费放行且不计入额度累计）。
// 顶部展示「渠道已用但未定价」提醒列表 —— admin 不用猜该配哪些价。

const fmtPrice = (n: number) => {
  // 单价展示：小数太多时保留足够精度，去掉尾零
  const s = n.toFixed(6).replace(/0+$/, '').replace(/\.$/, '')
  return s === '' ? '0' : s
}

export default function AdminPrices() {
  const { t } = useTranslation()
  const [rows, setRows] = useState<ModelPrice[]>([])
  const [unpriced, setUnpriced] = useState<string[]>([])
  const [kw, setKw] = useState('')
  const [err, setErr] = useState('')
  const [notice, setNotice] = useState('')
  const [loading, setLoading] = useState(true)
  // 编辑弹窗：null = 关闭；model 为空 = 新增
  const [editing, setEditing] = useState<{ model: string; prompt: string; completion: string; remark: string } | null>(null)
  const [saving, setSaving] = useState(false)
  const [deleting, setDeleting] = useState<ModelPrice | null>(null)
  // 列表分页（前端分页：条目量级几百，后端全量返回）
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(20)
  useEffect(() => { setPage(0) }, [rows, kw])
  // 一键同步（OpenRouter）：弹窗打开 = 拉预览中；items 就绪后展示勾选列表
  const [syncOpen, setSyncOpen] = useState(false)
  const [syncLoading, setSyncLoading] = useState(false)
  const [syncErr, setSyncErr] = useState('')
  const [syncItems, setSyncItems] = useState<PriceSyncPreviewItem[]>([])
  const [syncChecked, setSyncChecked] = useState<Set<string>>(new Set())
  const [syncKw, setSyncKw] = useState('')
  const [syncSaving, setSyncSaving] = useState(false)

  const reload = () => {
    setLoading(true)
    Promise.all([
      api.get<ModelPrice[]>('/api/admin/prices'),
      api.get<string[]>('/api/admin/prices/unpriced'),
    ])
      .then(([ps, un]) => {
        setRows(ps ?? [])
        setUnpriced(un ?? [])
      })
      .catch((e) => setErr(e.message))
      .finally(() => setLoading(false))
  }

  useEffect(reload, [])

  const filtered = useMemo(() => {
    const k = kw.trim().toLowerCase()
    if (!k) return rows
    return rows.filter((r) => r.model.toLowerCase().includes(k) || r.remark.toLowerCase().includes(k))
  }, [rows, kw])
  const pagedRows = useMemo(
    () => filtered.slice(page * pageSize, page * pageSize + pageSize),
    [filtered, page, pageSize],
  )

  const save = () => {
    if (!editing) return
    const body: ModelPriceUpsert = {
      model: editing.model.trim(),
      prompt_price: Number(editing.prompt),
      completion_price: Number(editing.completion),
      remark: editing.remark.trim(),
    }
    if (!body.model) {
      setErr(t('adminPrices.errModelRequired'))
      return
    }
    if (Number.isNaN(body.prompt_price) || Number.isNaN(body.completion_price) || body.prompt_price < 0 || body.completion_price < 0) {
      setErr(t('adminPrices.errNonNegative'))
      return
    }
    setSaving(true)
    setErr('')
    api.post<ModelPrice>('/api/admin/prices', body)
      .then(() => {
        setEditing(null)
        setNotice(t('adminPrices.savedMsg', { model: body.model }))
        reload()
      })
      .catch((e) => setErr(e.message))
      .finally(() => setSaving(false))
  }

  const doDelete = () => {
    if (!deleting) return
    api.del(`/api/admin/prices/${deleting.id}`)
      .then(() => {
        setNotice(t('adminPrices.deletedMsg', { model: deleting.model }))
        setDeleting(null)
        reload()
      })
      .catch((e) => setErr(e.message))
  }

  const openEdit = (r?: ModelPrice) => {
    setErr('')
    setEditing(r
      ? { model: r.model, prompt: String(r.prompt_price), completion: String(r.completion_price), remark: r.remark }
      : { model: '', prompt: '', completion: '', remark: '' })
  }

  const openNewFromUnpriced = (model: string) => {
    setNotice('')
    setEditing({ model, prompt: '', completion: '', remark: '' })
  }

  // ---- 一键同步（OpenRouter：preview 对比 → 勾选 → commit 写入）----
  const openSync = () => {
    setNotice('')
    setErr('')
    setSyncErr('')
    setSyncKw('')
    setSyncItems([])
    setSyncChecked(new Set())
    setSyncOpen(true)
    setSyncLoading(true)
    api.get<{ source: string; items: PriceSyncPreviewItem[] }>('/api/admin/prices/sync/preview')
      .then((d) => {
        const items = d?.items ?? []
        setSyncItems(items)
        // 默认勾选：新增 + 价格有变；与现价一致的不需要写
        setSyncChecked(new Set(items.filter((i) => i.status !== 'same').map((i) => i.model)))
      })
      .catch((e) => setSyncErr(e.message))
      .finally(() => setSyncLoading(false))
  }

  const toggleSync = (model: string) => {
    setSyncChecked((prev) => {
      const next = new Set(prev)
      if (next.has(model)) next.delete(model)
      else next.add(model)
      return next
    })
  }

  const commitSync = () => {
    const items = syncItems.filter((i) => syncChecked.has(i.model))
    if (items.length === 0) return
    setSyncSaving(true)
    setSyncErr('')
    api.post<{ saved: number }>('/api/admin/prices/sync', {
      items: items.map((i) => ({ model: i.model, prompt_price: i.prompt_price, completion_price: i.completion_price })),
    })
      .then((d) => {
        setSyncOpen(false)
        setNotice(t('adminPrices.syncDoneMsg', { n: d?.saved ?? items.length }))
        reload()
      })
      .catch((e) => setSyncErr(e.message))
      .finally(() => setSyncSaving(false))
  }

  return (
    <div>
      <div className="mb-1 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-lg font-semibold">{t('adminPrices.title')}</h2>
        <div className="flex gap-2">
          <button className="btn-ghost" onClick={openSync}>
            <CloudDownload className="h-4 w-4" /> {t('adminPrices.syncButton')}
          </button>
          <button className="btn-primary" onClick={() => openEdit()}>
            <Plus className="h-4 w-4" /> {t('adminPrices.addButton')}
          </button>
        </div>
      </div>
      <p className="mb-4 text-xs text-muted">
        {t('adminPrices.intro1')}<span className="font-mono">*</span>{t('adminPrices.intro2')}
      </p>

      {/* 未定价提醒 */}
      {unpriced.length > 0 && (
        <div className="warn-well mb-4 rounded-lg px-4 py-3 text-sm">
          <div className="mb-2 flex items-center gap-2 font-medium">
            <TriangleAlert className="h-4 w-4 text-amber-500" />
            {t('adminPrices.unpricedBanner', { n: unpriced.length })}
          </div>
          <div className="flex flex-wrap gap-1.5">
            {unpriced.map((m) => (
              <button key={m} className="badge badge-zinc cursor-pointer hover:opacity-80" title={t('adminPrices.addPriceTip')}
                onClick={() => openNewFromUnpriced(m)}>
                {m} +
              </button>
            ))}
          </div>
          <p className="mt-2 text-xs text-muted">{t('adminPrices.unpricedSource')}</p>
        </div>
      )}

      {err && <div className="err-well mb-3 rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400">{err}</div>}
      {notice && !err && <div className="mono-well mb-3 rounded-lg px-3 py-2 text-sm text-emerald-600 dark:text-emerald-400">{notice}</div>}

      <div className="mb-3 flex items-center gap-2">
        <Search className="h-4 w-4 text-muted" />
        <input className="input-sm max-w-xs" placeholder={t('adminPrices.searchPlaceholder')} value={kw} onChange={(e) => setKw(e.target.value)} />
      </div>

      <div className="card overflow-x-auto p-0">
        <table className="w-full text-sm">
          <thead>
            <tr className="thead-row text-left text-xs">
              <th className="px-4 py-3">{t('common.model')}</th>
              <th className="px-4 py-3 text-right">{t('adminPrices.colInPrice')}</th>
              <th className="px-4 py-3 text-right">{t('adminPrices.colOutPrice')}</th>
              <th className="px-4 py-3">{t('common.remark')}</th>
              <th className="px-4 py-3">{t('adminPrices.colUpdatedAt')}</th>
              <th className="px-4 py-3 text-right">{t('common.actions')}</th>
            </tr>
          </thead>
          <tbody>
            {pagedRows.map((r) => (
              <tr key={r.id} className="tbody-row">
                <td className="px-4 py-2.5">
                  {r.model === '*'
                    ? <span className="badge badge-yellow" title={t('adminPrices.fallbackTip')}><DollarSign className="h-3 w-3" /> {t('adminPrices.fallbackBadge')}</span>
                    : <span className="badge badge-zinc max-w-64 cursor-default truncate" title={r.model}>{r.model}</span>}
                </td>
                <td className="px-4 py-2.5 text-right font-mono tabular-nums">${fmtPrice(r.prompt_price)}</td>
                <td className="px-4 py-2.5 text-right font-mono tabular-nums">${fmtPrice(r.completion_price)}</td>
                <td className="max-w-52 cursor-default truncate px-4 py-2.5 text-muted" title={r.remark}>{r.remark || '—'}</td>
                <td className="whitespace-nowrap px-4 py-2.5 text-xs text-muted">{new Date(r.updated_at).toLocaleString()}</td>
                <td className="whitespace-nowrap px-4 py-2.5 text-right">
                  <button className="btn-ghost mr-1 h-7 px-2" onClick={() => openEdit(r)}><Pencil className="h-3.5 w-3.5" /> {t('common.edit')}</button>
                  <button className="btn-ghost h-7 px-2 text-red-500" onClick={() => setDeleting(r)}><Trash2 className="h-3.5 w-3.5" /> {t('common.delete')}</button>
                </td>
              </tr>
            ))}
            {pagedRows.length === 0 && !loading && (
              <tr><td colSpan={6} className="px-4 py-10 text-center text-muted">
                {kw ? t('adminPrices.noMatch') : t('adminPrices.emptyTable')}
              </td></tr>
            )}
          </tbody>
        </table>
      </div>
      {/* 分页条放在 overflow 容器外，避免随表格横向滚动 */}
      {filtered.length > 0 && (
        <Pagination className="mt-2"
          total={filtered.length} page={page} pageSize={pageSize}
          onPage={setPage}
          onPageSize={(n) => { setPageSize(n); setPage(0) }} />
      )}
      {loading && <div className="mt-2 text-center text-xs text-muted opacity-70">{t('common.loading')}</div>}

      {/* 新增 / 编辑弹窗 */}
      {editing && (
        <Modal title={rows.some((r) => r.model === editing.model) || editing.model !== '' ? t('adminPrices.edit') : t('adminPrices.addButton')} onClose={() => setEditing(null)}>
          <div className="space-y-3">
            <label className="block">
              <span className="mb-1 block text-xs text-muted">{t('adminPrices.labelModelA')} <span className="font-mono">*</span> {t('adminPrices.labelModelB')}</span>
              <input className="input w-full font-mono" value={editing.model} placeholder={t('adminPrices.modelPlaceholder')}
                onChange={(e) => setEditing({ ...editing, model: e.target.value })} />
            </label>
            <div className="grid grid-cols-2 gap-3">
              <label className="block">
                <span className="mb-1 block text-xs text-muted">{t('adminPrices.labelInPriceFull')}</span>
                <input className="input w-full font-mono" type="number" min="0" step="any" value={editing.prompt} placeholder="2.5"
                  onChange={(e) => setEditing({ ...editing, prompt: e.target.value })} />
              </label>
              <label className="block">
                <span className="mb-1 block text-xs text-muted">{t('adminPrices.labelOutPriceFull')}</span>
                <input className="input w-full font-mono" type="number" min="0" step="any" value={editing.completion} placeholder="10"
                  onChange={(e) => setEditing({ ...editing, completion: e.target.value })} />
              </label>
            </div>
            <label className="block">
              <span className="mb-1 block text-xs text-muted">{t('adminPrices.labelRemark')}</span>
              <input className="input w-full" value={editing.remark} placeholder={t('adminPrices.remarkPlaceholder')}
                onChange={(e) => setEditing({ ...editing, remark: e.target.value })} />
            </label>
            <p className="text-xs text-muted">
              {t('adminPrices.formulaExample')}
            </p>
            <div className="flex justify-end gap-2 pt-1">
              <button className="btn-ghost" onClick={() => setEditing(null)}>{t('common.cancel')}</button>
              <button className="btn-primary" disabled={saving} onClick={save}>{saving ? t('adminPrices.saving') : t('common.save')}</button>
            </div>
          </div>
        </Modal>
      )}

      {/* 删除确认 */}
      {deleting && (
        <Modal title={t('adminPrices.deleteTitle')} onClose={() => setDeleting(null)}>
          <p className="text-sm">
            {t('adminPrices.deleteConfirmA')}<span className="font-mono font-medium">{deleting.model}</span>{t('adminPrices.deleteConfirmB')}
          </p>
          <div className="mt-4 flex justify-end gap-2">
            <button className="btn-ghost" onClick={() => setDeleting(null)}>{t('common.cancel')}</button>
            <button className="btn-primary" style={{ background: '#dc2626' }} onClick={doDelete}>{t('common.delete')}</button>
          </div>
        </Modal>
      )}

      {/* 一键同步（OpenRouter）弹窗 */}
      {syncOpen && (
        <Modal title={t('adminPrices.syncModalTitle')} wide onClose={() => !syncSaving && setSyncOpen(false)}>
          <div className="space-y-3">
            <p className="text-xs text-muted">
              {t('adminPrices.syncIntro')}
            </p>
            {syncErr && <div className="err-well rounded-lg px-3 py-2 text-sm text-red-600 dark:text-red-400">{syncErr}</div>}
            {syncLoading && <div className="py-10 text-center text-sm text-muted">{t('adminPrices.syncLoadingText')}</div>}
            {!syncLoading && syncItems.length > 0 && (
              <>
                <div className="flex items-center gap-2">
                  <Search className="h-4 w-4 shrink-0 text-muted" />
                  <input className="input-sm w-full" placeholder={t('adminPrices.filterPlaceholder')}
                    value={syncKw} onChange={(e) => setSyncKw(e.target.value)} />
                </div>
                <div className="max-h-80 overflow-y-auto rounded-lg border border-black/10 dark:border-white/10">
                  <table className="w-full text-sm">
                    <thead className="sticky top-0 bg-white dark:bg-zinc-900">
                      <tr className="thead-row text-left text-xs">
                        <th className="w-8 px-2 py-2"></th>
                        <th className="px-2 py-2">{t('common.model')}</th>
                        <th className="px-2 py-2 text-right">{t('adminPrices.syncColIn')}</th>
                        <th className="px-2 py-2 text-right">{t('adminPrices.syncColOut')}</th>
                        <th className="px-2 py-2">{t('common.status')}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {syncItems
                        .filter((i) => !syncKw.trim() || i.model.toLowerCase().includes(syncKw.trim().toLowerCase()))
                        .map((i) => (
                          <tr key={i.model} className="tbody-row">
                            <td className="px-2 py-2">
                              <input type="checkbox" className="accent-violet-600" disabled={i.status === 'same'}
                                checked={syncChecked.has(i.model)} onChange={() => toggleSync(i.model)} />
                            </td>
                            <td className="max-w-44 cursor-default truncate px-2 py-2 font-mono text-xs" title={`${i.model}（${i.source_id}）`}>{i.model}</td>
                            <td className="px-2 py-2 text-right font-mono tabular-nums">${fmtPrice(i.prompt_price)}</td>
                            <td className="px-2 py-2 text-right font-mono tabular-nums">${fmtPrice(i.completion_price)}</td>
                            <td className="whitespace-nowrap px-2 py-2">
                              {i.status === 'new' && <span className="badge badge-green">{t('adminPrices.statusNew')}</span>}
                              {i.status === 'update' && (
                                <span className="badge badge-yellow" title={t('adminPrices.currentPriceTip', { from: fmtPrice(i.current_prompt_price ?? 0), to: fmtPrice(i.prompt_price) })}>
                                  {t('adminPrices.statusUpdate')}
                                </span>
                              )}
                              {i.status === 'same' && <span className="badge badge-zinc">{t('adminPrices.statusSame')}</span>}
                            </td>
                          </tr>
                        ))}
                      {syncItems.filter((i) => !syncKw.trim() || i.model.toLowerCase().includes(syncKw.trim().toLowerCase())).length === 0 && (
                        <tr><td colSpan={5} className="px-4 py-6 text-center text-muted">{t('adminPrices.noMatch')}</td></tr>
                      )}
                    </tbody>
                  </table>
                </div>
                <div className="flex items-center justify-between text-xs text-muted">
                  <span>{t('adminPrices.syncSummary', {
                    total: syncItems.length,
                    created: syncItems.filter((i) => i.status === 'new').length,
                    updated: syncItems.filter((i) => i.status === 'update').length,
                    same: syncItems.filter((i) => i.status === 'same').length,
                  })}</span>
                  <span>{t('adminPrices.syncSelected', { n: syncChecked.size })}</span>
                </div>
                <div className="flex justify-end gap-2 pt-1">
                  <button className="btn-ghost" disabled={syncSaving} onClick={() => setSyncOpen(false)}>{t('common.cancel')}</button>
                  <button className="btn-primary" disabled={syncSaving || syncChecked.size === 0} onClick={commitSync}>
                    {syncSaving ? t('adminPrices.importing') : t('adminPrices.importChecked', { n: syncChecked.size })}
                  </button>
                </div>
              </>
            )}
            {!syncLoading && syncItems.length === 0 && !syncErr && (
              <div className="py-6 text-center text-sm text-muted">{t('adminPrices.syncEmpty')}</div>
            )}
          </div>
        </Modal>
      )}
    </div>
  )
}
