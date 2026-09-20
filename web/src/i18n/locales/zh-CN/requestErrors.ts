// 失败请求详情（渠道健康视图共用组件）
export default {
  title: '最近失败详情',
  hint: '只记失败元数据与上游错误摘要（最多 512 字符，绝不含 prompt 与响应正文）；单渠道每分钟最多 30 条，保留 7 天',
  empty: '暂无失败记录',
  noProvider: '（不限渠道）',
  colTime: '时间',
  colKind: '分类',
  colStatus: '状态码',
  colModel: '模型',
  colKey: 'API Key',
  colLatency: '耗时',
  colMessage: '错误详情',

  // 失败分类
  kindNoChannel: '无可用渠道',
  kindCircuitOpen: '熔断中',
  kindCredential: '凭据问题',
  kindTransform: '协议转换失败',
  kindProxy: '代理配置',
  kindUpstream4xx: '上游 4xx',
  kindUpstream429: '上游限流',
  kindUpstream5xx: '上游 5xx',
  kindTransport: '网络/连接',
  kindStreamAborted: '流中断',
}
