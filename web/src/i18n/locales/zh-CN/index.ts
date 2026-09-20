// 中文文案聚合：各域独立文件，新增域时在此注册
import common from './common'
import app from './app'
import login from './login'
import providers from './providers'
import keys from './keys'
import usage from './usage'
import settings from './settings'
import adminUsers from './adminUsers'
import adminUsage from './adminUsage'
import adminPrices from './adminPrices'
import adminSettings from './adminSettings'
import adminProviders from './adminProviders'
import requestErrors from './requestErrors'

export default {
  common,
  app,
  login,
  providers,
  keys,
  usage,
  settings,
  adminUsers,
  adminUsage,
  adminPrices,
  adminSettings,
  adminProviders,
  requestErrors,
}
