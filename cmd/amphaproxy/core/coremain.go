package core

//var ampHAProxyControllerVersion string

//Run launch main loop
func Run(version string, build string) {
	//ampHAProxyControllerVersion = version
	conf.load(version, build)
	haproxy.init()
	haproxy.trapSignal()
	initAPI()
	haproxy.start()
}
