import request from './request'

// 维修状态查询接口。
export const statusApi = {
  overview: () => request.get('/status/overview'),
  lamps: (params) => request.get('/status/lamps', { params }),
  track: (params) => request.get('/status/track', { params }),
  // 导出维修状态清单 CSV, 与列表共用同一套过滤条件。
  exportLamps: (params) => request.get('/status/lamps/export', { params, responseType: 'blob' }),
}
