import request from './request'

// 维修状态查询接口。
export const statusApi = {
  overview: () => request.get('/status/overview'),
  lamps: (params) => request.get('/status/lamps', { params }),
  track: (params) => request.get('/status/track', { params }),
  // 导出 CSV: 走与清单相同的过滤条件, 以 Blob 形式返回文件内容。
  exportLamps: (params) =>
    request.get('/status/lamps/export', { params, responseType: 'blob' }),
}
