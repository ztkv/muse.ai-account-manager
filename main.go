// main.go — muse.ai 账号管理系统入口（Web 服务 + 命令行工具）。
//
// 用法：
//   muse-manager serve   [-addr 127.0.0.1:8787] [-data data] [-open]
//   muse-manager add     -email xx [-password ..] [-apikey ..] [-plan free] [-status active] ...
//   muse-manager list    [-data data]
//   muse-manager show    <ID或邮箱关键词> [-data data] [-reveal]
//   muse-manager invite  <ID或邮箱关键词> [-data data]   拉取邀请信息（需先录入 Cookie）
//   muse-manager check   <ID或邮箱关键词> [-data data]   准入体检（auth/check）
//   muse-manager redeem <ID或邮箱关键词> <邀请码> [-data data]  兑换邀请码（自动 VM 唤醒）
//   muse-manager login <邮箱> [-code xxx] [-save] [-data data]  验证码登录取 Cookie
//   muse-manager set-proxy [socks5://…] [-data data]    设置/清除全局默认代理
//   muse-manager delete  <ID> [-data data] [-f]
//   muse-manager export  [-data data] [-out backup.json] [-reveal]
//   muse-manager import  -in backup.json [-data data] [-mode merge]
//
// 可选环境变量：
//   MUSE_ACCESS_TOKEN  Web 访问令牌（设置后页面需先登录）
//   MUSE_MASTER_KEY    主密钥（hex 32 字节或任意字符串）；默认自动生成 key.bin
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"
)

func printUsage() {
	fmt.Print(`
muse.ai 账号管理系统

命令:
  serve    启动 Web 管理界面        serve [-addr 127.0.0.1:8787] [-data data] [-open]
  add      命令行新增账号            add -email a@b.c [-login-type email_code] [-code-email xx]
                                     [-password ..] [-apikey ..] [-plan free]
                                     [-status active] [-balance ..] [-expires 2026-12-31]
                                     [-invite ..] [-tags 标签1,标签2] [-notes ..]
  list     命令行列出账号            list [-data data]
  show     查看账号详情              show <ID或邮箱关键词> [-data data] [-reveal]
  invite   拉取邀请信息              invite <ID或邮箱关键词> [-data data]
  check    账号准入体检              check <ID或邮箱关键词> [-data data]
  redeem   兑换邀请码                redeem <ID或邮箱关键词> <邀请码> [-data data]
                                      （自动执行 VM 唤醒→stateful-sharing→兑换→取回自己的码）
  login    验证码登录取 Cookie       login <邮箱> [-code 123456] [-proxy url] [-data data] [-save]
                                      （成功打印完整 Cookie；-save 同时写入/更新同邮箱账号）
  set-proxy 设置全局默认代理         set-proxy socks5://127.0.0.1:10808 [-data data]
                                     （不带参数=清除；账号代理优先于全局代理）
  delete   删除账号                  delete <ID> [-data data] [-f]
  export   导出备份 JSON             export [-data data] [-out muse-backup.json] [-reveal]
  import   导入备份 JSON             import -in backup.json [-data data] [-mode merge]
  help     显示本帮助

环境变量:
  MUSE_ACCESS_TOKEN   设置后 Web 界面需要访问令牌登录
  MUSE_MASTER_KEY     主密钥（32 字节 hex 或任意字符串）；缺省自动生成 data/key.bin
`)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	var err error
	switch strings.ToLower(os.Args[1]) {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "add":
		err = cmdAdd(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "show":
		err = cmdShow(os.Args[2:])
	case "invite":
		err = cmdInvite(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "redeem":
		err = cmdRedeem(os.Args[2:])
	case "login":
		err = cmdLogin(os.Args[2:])
	case "set-proxy":
		err = cmdSetProxy(os.Args[2:])
	case "delete":
		err = cmdDelete(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "help", "-h", "--help", "/?":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// reorderForFlagParse 解决 Go flag 包“遇到第一个位置参数就停止解析”的限制：
// 把所有 -flag 参数（含取值）挪到前面，位置参数放后面，任意顺序均可解析。
func reorderForFlagParse(fs *flag.FlagSet, args []string) []string {
	var flagArgs, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 1 && (a[0] == '-') {
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") {
				flagArgs = append(flagArgs, a)
				continue
			}
			f := fs.Lookup(name)
			flagArgs = append(flagArgs, a)
			if f != nil {
				isBool := false
				if bv, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
					isBool = bv.IsBoolFlag()
				}
				if !isBool && i+1 < len(args) {
					flagArgs = append(flagArgs, args[i+1])
					i++
				}
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flagArgs, pos...)
}

func openStore(dataDir string) (*Store, error) { return OpenStore(dataDir) }

// ---------- serve ----------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "监听地址（建议仅本机）")
	data := fs.String("data", "data", "数据目录")
	open := fs.Bool("open", false, "启动后自动打开浏览器")
	_ = fs.Parse(reorderForFlagParse(fs, args))

	store, err := openStore(*data)
	if err != nil {
		return err
	}
	crypto, err := NewCryptoFromDataDir(*data)
	if err != nil {
		return err
	}
	srv := &Server{store: store, crypto: crypto, token: strings.TrimSpace(os.Getenv("MUSE_ACCESS_TOKEN")), logins: newLoginSessions()}

	url := "http://" + *addr
	fmt.Println("┌─────────────────────────────────────────────┐")
	fmt.Println("│        muse.ai 账号管理系统  (Go)           │")
	fmt.Println("└─────────────────────────────────────────────┘")
	fmt.Println("  地址     :", url)
	fmt.Println("  数据目录 :", absPath(*data))
	if srv.token != "" {
		fmt.Println("  访问令牌 : 已启用（打开页面后需登录）")
	} else {
		fmt.Println("  访问令牌 : 未启用（设置 MUSE_ACCESS_TOKEN 可加保护）")
	}
	switch {
	case strings.TrimSpace(store.GetSettings().Proxy) != "":
		fmt.Println("  全局代理 :", store.GetSettings().Proxy, "（settings.json，页面右上角「⚙ 代理」可改）")
	case strings.TrimSpace(os.Getenv("MUSE_PROXY")) != "":
		fmt.Println("  全局代理 :", os.Getenv("MUSE_PROXY"), "（环境变量 MUSE_PROXY）")
	default:
		fmt.Println("  全局代理 : 未设置——muse.ai 请求将直连（国内通常不可达，请点页面右上角「⚙ 代理」设置）")
	}
	fmt.Println("  快捷键   : Ctrl+C 停止服务")
	if *open {
		openBrowser(url)
	}
	return http.ListenAndServe(*addr, srv.Routes())
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// ---------- add ----------

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	data := fs.String("data", "data", "数据目录")
	email := fs.String("email", "", "登录邮箱/用户名（必填）")
	loginType := fs.String("login-type", "email_code", "登录方式: email_code=邮箱验证码 / password=密码 / both")
	codeEmail := fs.String("code-email", "", "接收验证码的邮箱（别名/catch-all）")
	password := fs.String("password", "", "登录密码（验证码登录可留空）")
	apikey := fs.String("apikey", "", "API Key / Token")
	cookie := fs.String("cookie", "", "muse.ai 登录 Cookie（含 hatch_sess，用于拉取邀请信息）")
	proxy := fs.String("proxy", "", "本账号请求代理（http:// 或 socks5://，可选）")
	invite := fs.String("invite", "", "邀请码")
	plan := fs.String("plan", "free", "订阅计划: free/plus/pro/enterprise")
	status := fs.String("status", "active", "状态: active/expiring/banned/unverified/cooldown")
	balance := fs.String("balance", "", "额度/Token 余额")
	expires := fs.String("expires", "", "到期时间 YYYY-MM-DD")
	tags := fs.String("tags", "", "标签，逗号分隔")
	notes := fs.String("notes", "", "备注")
	_ = fs.Parse(reorderForFlagParse(fs, args))

	if strings.TrimSpace(*email) == "" {
		return fmt.Errorf("-email 必填")
	}
	switch strings.ToLower(*loginType) {
	case "email_code", "password", "both":
		*loginType = strings.ToLower(*loginType)
	default:
		return fmt.Errorf("-login-type 仅支持 email_code / password / both")
	}
	if *expires != "" {
		if _, err := time.Parse("2006-01-02", *expires); err != nil {
			return fmt.Errorf("-expires 格式应为 YYYY-MM-DD")
		}
	}
	var tagList []string
	for _, t := range strings.Split(*tags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tagList = append(tagList, t)
		}
	}
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	crypto, err := NewCryptoFromDataDir(*data)
	if err != nil {
		return err
	}
	a := &Account{
		Email:      *email,
		LoginType:  *loginType,
		CodeEmail:  *codeEmail,
		Password:   crypto.Encrypt(*password),
		APIKey:     crypto.Encrypt(*apikey),
		Cookie:     crypto.Encrypt(*cookie),
		Proxy:      *proxy,
		InviteCode: *invite,
		Plan:       *plan,
		Status:     *status,
		Balance:    *balance,
		ExpiresAt:  *expires,
		Tags:       tagList,
		Notes:      *notes,
	}
	if err := store.Add(a); err != nil {
		return err
	}
	fmt.Printf("已添加: %s  (ID: %s)\n", a.Email, a.ID)
	return nil
}

// ---------- list ----------

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	data := fs.String("data", "data", "数据目录")
	_ = fs.Parse(reorderForFlagParse(fs, args))
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	list := store.List()
	if len(list) == 0 {
		fmt.Println("(暂无账号)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\t邮箱\t登录\t计划\t状态\t余额\t到期\t标签")
	for _, a := range list {
		lt := map[string]string{"email_code": "验证码", "password": "密码", "both": "密码+验证码"}[a.LoginType]
		if lt == "" {
			lt = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			a.ID, a.Email, lt, a.Plan, a.Status, a.Balance, a.ExpiresAt, strings.Join(a.Tags, ","))
	}
	return tw.Flush()
}

// ---------- show ----------

func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	data := fs.String("data", "data", "数据目录")
	reveal := fs.Bool("reveal", false, "显示密码/API Key 明文")
	_ = fs.Parse(reorderForFlagParse(fs, args))
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: muse-manager show <ID或邮箱关键词>")
	}
	kw := fs.Arg(0)
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	crypto, err := NewCryptoFromDataDir(*data)
	if err != nil {
		return err
	}
	var hit *Account
	for _, a := range store.List() {
		if strings.HasPrefix(a.ID, kw) || strings.EqualFold(a.Email, kw) ||
			strings.Contains(strings.ToLower(a.Email), strings.ToLower(kw)) {
			hit = a
			break
		}
	}
	if hit == nil {
		return fmt.Errorf("未找到匹配 %q 的账号", kw)
	}
	pw, err1 := crypto.Decrypt(hit.Password)
	key, err2 := crypto.Decrypt(hit.APIKey)
	if !*reveal {
		if hit.Password != "" {
			pw = "********"
		}
		if hit.APIKey != "" {
			key = "********"
		}
	}
	fmt.Println("ID        :", hit.ID)
	fmt.Println("邮箱      :", hit.Email)
	fmt.Println("登录方式  :", loginTypeLabel(hit.LoginType))
	if hit.CodeEmail != "" {
		fmt.Println("验证码邮箱:", hit.CodeEmail)
	}
	fmt.Println("密码      :", pwOrHint(pw, err1))
	fmt.Println("API Key   :", pwOrHint(key, err2))
	fmt.Println("邀请码    :", hit.InviteCode)
	fmt.Println("计划      :", hit.Plan)
	fmt.Println("状态      :", hit.Status)
	fmt.Println("余额      :", hit.Balance)
	fmt.Println("到期      :", hit.ExpiresAt)
	fmt.Println("标签      :", strings.Join(hit.Tags, ", "))
	fmt.Println("备注      :", hit.Notes)
	fmt.Println("最后使用  :", hit.LastUsedAt)
	fmt.Println("创建时间  :", hit.CreatedAt)
	return nil
}

func pwOrHint(v string, err error) string {
	if err != nil {
		return "<解密失败: " + err.Error() + ">"
	}
	if v == "" {
		return "(未设置)"
	}
	return v
}

func loginTypeLabel(lt string) string {
	switch lt {
	case "email_code":
		return "邮箱验证码"
	case "password":
		return "密码登录"
	case "both":
		return "密码 + 验证码"
	}
	return "—"
}

func parseInviteJSON(raw string) (*MuseInvite, error) {
	var inv MuseInvite
	if err := json.Unmarshal([]byte(raw), &inv); err != nil {
		return nil, err
	}
	return &inv, nil
}

// cmdCheck 准入体检：POST /api/auth/check，结果映射到账号状态。
func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	data := fs.String("data", "data", "数据目录")
	_ = fs.Parse(reorderForFlagParse(fs, args))
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: muse-manager check <ID或邮箱关键词>")
	}
	kw := fs.Arg(0)
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	crypto, err := NewCryptoFromDataDir(*data)
	if err != nil {
		return err
	}
	var hit *Account
	for _, a := range store.List() {
		if strings.HasPrefix(a.ID, kw) || strings.EqualFold(a.Email, kw) ||
			strings.Contains(strings.ToLower(a.Email), strings.ToLower(kw)) {
			hit = a
			break
		}
	}
	if hit == nil {
		return fmt.Errorf("未找到匹配 %q 的账号", kw)
	}
	cookie, derr := crypto.Decrypt(hit.Cookie)
	if derr != nil {
		return fmt.Errorf("Cookie 解密失败: %w", derr)
	}
	fmt.Printf("正在体检 %s …\n", hit.Email)
	res, ferr := CheckAdmission(cookie, accountIdentity(hit), hit.Proxy, store.GetSettings().Proxy)
	if ferr != nil {
		return ferr
	}
	checked := now()
	checkJSON, _ := json.Marshal(res)
	newStatus := ""
	switch res.State {
	case "admitted":
		newStatus = "active"
	case "disabled":
		newStatus = "banned"
	case "region_limited":
		newStatus = "region_limited"
	case "auth_expired":
		newStatus = "unverified"
	}
	if uerr := store.Update(hit.ID, func(x *Account) error {
		x.CheckInfo = string(checkJSON)
		x.CheckedAt = checked
		if newStatus != "" {
			x.Status = newStatus
		}
		return nil
	}); uerr != nil {
		return uerr
	}
	fmt.Printf("检测结果: [%s] %s\n", res.State, res.Detail)
	if res.VMState != "" {
		fmt.Println("VM 状态 :", res.VMState)
	}
	if newStatus != "" {
		fmt.Printf("账号状态已更新: %s → %s\n", hit.Status, newStatus)
	} else {
		fmt.Println("账号状态保持不变（结果无法映射）")
	}
	fmt.Println("检测时间:", checked)
	return nil
}

// cmdRedeem 兑换邀请码：VM 唤醒 → stateful-sharing → redeem → 取回自己的码。
func cmdRedeem(args []string) error {
	fs := flag.NewFlagSet("redeem", flag.ExitOnError)
	data := fs.String("data", "data", "数据目录")
	_ = fs.Parse(reorderForFlagParse(fs, args))
	if fs.NArg() < 2 {
		return fmt.Errorf("用法: muse-manager redeem <ID或邮箱关键词> <邀请码>")
	}
	kw := fs.Arg(0)
	code := strings.ToUpper(strings.TrimSpace(fs.Arg(1)))
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	crypto, err := NewCryptoFromDataDir(*data)
	if err != nil {
		return err
	}
	var hit *Account
	for _, a := range store.List() {
		if strings.HasPrefix(a.ID, kw) || strings.EqualFold(a.Email, kw) ||
			strings.Contains(strings.ToLower(a.Email), strings.ToLower(kw)) {
			hit = a
			break
		}
	}
	if hit == nil {
		return fmt.Errorf("未找到匹配 %q 的账号", kw)
	}
	cookie, derr := crypto.Decrypt(hit.Cookie)
	if derr != nil {
		return fmt.Errorf("Cookie 解密失败: %w", derr)
	}
	fmt.Printf("正在兑换 %s 的邀请码 %s …（VM 唤醒 → 确认 → 兑换）\n", hit.Email, code)
	out, rerr := RedeemInviteCode(cookie, accountIdentity(hit), code, hit.Proxy, store.GetSettings().Proxy)
	if rerr != nil {
		return rerr
	}
	redeemed := now()
	outJSON, _ := json.Marshal(out)
	newStatus := ""
	switch out.State {
	case "redeemed":
		newStatus = "active"
	case "auth_expired":
		newStatus = "unverified"
	case "region_limited":
		newStatus = "region_limited"
	}
	if uerr := store.Update(hit.ID, func(x *Account) error {
		x.RedeemInfo = string(outJSON)
		x.RedeemedAt = redeemed
		if newStatus != "" {
			x.Status = newStatus
		}
		return nil
	}); uerr != nil {
		return uerr
	}
	fmt.Printf("兑换结果: [%s] %s\n", out.State, out.Detail)
	if out.VMID != "" {
		fmt.Println("VM ID    :", out.VMID)
	}
	if out.OwnCode != "" {
		fmt.Println("自己的码 :", out.OwnCode)
	}
	if newStatus != "" {
		fmt.Println("账号状态 →", newStatus)
	}
	fmt.Println("时间     :", redeemed)
	return nil
}

// cmdLogin 验证码登录存量账号，直接获取完整 Cookie（可选写入账号库）。
func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	data := fs.String("data", "data", "数据目录")
	code := fs.String("code", "", "6 位验证码（不传则交互输入）")
	proxy := fs.String("proxy", "", "本次登录使用的代理（留空用全局设置）")
	save := fs.Bool("save", false, "成功后写入/更新同邮箱账号的 Cookie")
	_ = fs.Parse(reorderForFlagParse(fs, args))
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: muse-manager login <邮箱> [-code 123456] [-save]")
	}
	email := strings.ToLower(strings.TrimSpace(fs.Arg(0)))
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	sessions := newLoginSessions()
	fmt.Printf("正在向 %s 发送验证码…\n", email)
	sid, hasAccount, err := sessions.LoginSendOTP(email, *proxy)
	if err != nil {
		return err
	}
	if !*hasAccount {
		fmt.Println("提示: 服务端未在该邮箱下发现 muse 账号（has_muse_account=false），仍可继续尝试")
	}
	otp := strings.TrimSpace(*code)
	if otp == "" {
		fmt.Print("请输入收到的 6 位验证码: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		ootp := strings.TrimSpace(line)
		if ootp == "" {
			return fmt.Errorf("未输入验证码")
		}
		ootp = strings.TrimSpace(ootp)
		otp = ootp
	}
	res, err := sessions.LoginVerify(sid, otp)
	if err != nil {
		return err
	}
	fmt.Println("登录成功！")
	fmt.Println("  账号 ID    :", res.AccountID)
	fmt.Println("  hatch_sess 长度:", res.HatchSessLen)
	fmt.Println("  Cookie     :", res.Cookie)
	if *save {
		var hit *Account
		for _, a := range store.List() {
			if strings.EqualFold(a.Email, email) {
				hit = a
				break
			}
		}
		crypto, cerr := NewCryptoFromDataDir(*data)
		if cerr != nil {
			return cerr
		}
		encCookie := crypto.Encrypt(res.Cookie)
		if hit != nil {
			if uerr := store.Update(hit.ID, func(x *Account) error {
				x.Cookie = encCookie
				x.Status = "active"
				return nil
			}); uerr != nil {
				return uerr
			}
			fmt.Println("已更新账号 Cookie:", hit.Email)
		} else {
			a := &Account{
				Email:     email,
				LoginType: "email_code",
				CodeEmail: email,
				Cookie:    encCookie,
				Plan:      "free",
				Status:    "active",
			}
			if aerr := store.Add(a); aerr != nil {
				return aerr
			}
			fmt.Println("已创建账号并保存 Cookie:", email)
		}
	}
	return nil
}

// cmdSetProxy 设置/清除全局默认代理（写入 <data>/settings.json，对所有账号生效）。
func cmdSetProxy(args []string) error {
	fs := flag.NewFlagSet("set-proxy", flag.ExitOnError)
	data := fs.String("data", "data", "数据目录")
	_ = fs.Parse(reorderForFlagParse(fs, args))
	proxy := ""
	if fs.NArg() > 0 {
		proxy = fs.Arg(0)
	}
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	if err := store.SaveSettings(Settings{Proxy: proxy}); err != nil {
		return err
	}
	if proxy == "" {
		fmt.Println("已清除全局代理（muse.ai 请求将直连或走 MUSE_PROXY 环境变量）")
	} else {
		fmt.Println("全局代理已设置:", proxy)
		fmt.Println("优先级：账号代理 > 全局代理 > MUSE_PROXY 环境变量")
	}
	return nil
}

// cmdInvite 拉取指定账号的 muse.ai 邀请信息并保存快照。
func cmdInvite(args []string) error {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	data := fs.String("data", "data", "数据目录")
	_ = fs.Parse(reorderForFlagParse(fs, args))
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: muse-manager invite <ID或邮箱关键词>")
	}
	kw := fs.Arg(0)
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	crypto, err := NewCryptoFromDataDir(*data)
	if err != nil {
		return err
	}
	var hit *Account
	for _, a := range store.List() {
		if strings.HasPrefix(a.ID, kw) || strings.EqualFold(a.Email, kw) ||
			strings.Contains(strings.ToLower(a.Email), strings.ToLower(kw)) {
			hit = a
			break
		}
	}
	if hit == nil {
		return fmt.Errorf("未找到匹配 %q 的账号", kw)
	}
	cookie, derr := crypto.Decrypt(hit.Cookie)
	if derr != nil {
		return fmt.Errorf("Cookie 解密失败: %w", derr)
	}
	fmt.Printf("正在拉取 %s 的邀请信息…\n", hit.Email)
	pubKey, inv, raw, ferr := FetchInvite(cookie, accountIdentity(hit), hit.Proxy, store.GetSettings().Proxy)
	if ferr != nil {
		return ferr
	}
	synced := now()
	if uerr := store.Update(hit.ID, func(x *Account) error {
		x.InviteInfo = string(raw)
		x.InviteSyncedAt = synced
		if inv.Code != "" {
			x.InviteCode = inv.Code
		}
		return nil
	}); uerr != nil {
		return uerr
	}
	if pubKey != "" {
		fmt.Println("Stripe 公钥:", pubKey)
	}
	fmt.Println("同步成功:", inv.Summary())
	fmt.Println("同步时间:", synced)
	return nil
}

// ---------- delete ----------

func cmdDelete(args []string) error {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	data := fs.String("data", "data", "数据目录")
	force := fs.Bool("f", false, "跳过确认")
	_ = fs.Parse(reorderForFlagParse(fs, args))
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: muse-manager delete <ID>")
	}
	id := fs.Arg(0)
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	a, err := store.Get(id)
	if err != nil {
		return fmt.Errorf("账号 %s 不存在", id)
	}
	if !*force {
		fmt.Printf("确认删除账号 %s (%s)? [y/N]: ", a.ID, a.Email)
		var ans string
		_, _ = fmt.Scanln(&ans)
		ans = strings.ToLower(strings.TrimSpace(ans))
		if ans != "y" && ans != "yes" {
			fmt.Println("已取消")
			return nil
		}
	}
	if err := store.Delete(id); err != nil {
		return err
	}
	fmt.Println("已删除:", a.Email)
	return nil
}

// ---------- export ----------

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	data := fs.String("data", "data", "数据目录")
	out := fs.String("out", "", "输出文件（默认 muse-backup-时间戳.json）")
	reveal := fs.Bool("reveal", false, "导出明文密码（敏感！）")
	_ = fs.Parse(reorderForFlagParse(fs, args))
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	crypto, err := NewCryptoFromDataDir(*data)
	if err != nil {
		return err
	}
	list := store.List()
	accs := make([]*Account, 0, len(list))
	for _, a := range list {
		cp := *a
		if *reveal {
			for _, v := range []*string{&cp.Password, &cp.APIKey} {
				if pt, e := crypto.Decrypt(*v); e == nil {
					*v = pt
				}
			}
		}
		accs = append(accs, &cp)
	}
	if *out == "" {
		*out = fmt.Sprintf("muse-backup-%s.json", time.Now().Format("20060102-150405"))
	}
	b, err := json.MarshalIndent(DBFile{Version: 1, UpdatedAt: now(), Accounts: accs}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, b, 0o600); err != nil {
		return err
	}
	fmt.Printf("已导出 %d 个账号 -> %s（%s）\n", len(accs), absPath(*out),
		map[bool]string{true: "含明文密码，请妥善保管", false: "密码为密文"}[*reveal])
	return nil
}

// ---------- import ----------

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	data := fs.String("data", "data", "数据目录")
	in := fs.String("in", "", "导入文件（必填）")
	mode := fs.String("mode", "merge", "合并模式: merge=跳过重复 / replace=整体替换")
	_ = fs.Parse(reorderForFlagParse(fs, args))
	if *in == "" {
		return fmt.Errorf("-in 必填")
	}
	b, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	var db DBFile
	if err := json.Unmarshal(b, &db); err != nil {
		return fmt.Errorf("解析 %s 失败: %w", *in, err)
	}
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	crypto, err := NewCryptoFromDataDir(*data)
	if err != nil {
		return err
	}
	for _, a := range db.Accounts {
		for _, f := range []*string{&a.Password, &a.APIKey} {
			if *f != "" && !IsEncrypted(*f) {
				*f = crypto.Encrypt(*f)
			}
		}
	}
	switch *mode {
	case "merge":
		added, skipped, err := store.MergeImport(db.Accounts)
		if err != nil {
			return err
		}
		fmt.Printf("合并导入完成: 新增 %d，跳过 %d，现共 %d 个账号\n", added, skipped, store.Count())
	case "replace":
		imported, err := store.ReplaceImport(db.Accounts)
		if err != nil {
			return err
		}
		fmt.Printf("整体替换完成: 导入 %d 个账号\n", imported)
	default:
		return fmt.Errorf("-mode 仅支持 merge / replace")
	}
	return nil
}
