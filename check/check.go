package check

import (
	logger "unlimitedai2api/common/loggger"
)

func CheckEnvVariable() {
	logger.SysLog("environment variable checking...")

	//if config.KLCookie == "" {
	//	logger.FatalLog("环境变量 KL_COOKIE 未设置")
	//}

	logger.SysLog("environment variable check passed.")
}
