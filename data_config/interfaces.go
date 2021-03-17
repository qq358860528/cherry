package cherryDataConfig

type (
	// IDataConfig 数据配置接口
	IDataConfig interface {
		Register(configFile ...IConfigFile)                   // 注册映射文件
		GetBytes(configName string) (data []byte, found bool) // 获取原始的数据
		GetParser() IDataParser                               // 当前参数配置的数据格式解析器
		GetDataSource() IDataSource                           // 当前参数配置的获取数据源
	}

	// IDataParser 数据格式解析接口
	IDataParser interface {
		TypeName() string                           // 注册名称
		Unmarshal(text []byte, v interface{}) error // 文件格式解析器
	}

	// IDataSource 配置文件数据源
	IDataSource interface {
		Name() string                                           // 数据源名称
		Init(dataConfig IDataConfig)                            // 函数初始化时
		ReadBytes(configName string) (data []byte, error error) // 获取数据流
		OnChange(fn ChangeFileFn)                               // 数据变更时
		Stop()                                                  // 停止
	}

	// ChangeFileFn 数据变更时触发该函数
	ChangeFileFn func(configName string, data []byte)

	// IConfigFile 配置文件接口
	IConfigFile interface {
		Name() string                             // 配置名称
		Load(maps interface{}, reload bool) error // 配置序列化后，执行该函数
	}
)
