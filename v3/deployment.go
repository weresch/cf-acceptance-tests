package v3

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cloudfoundry/cf-acceptance-tests/helpers/app_helpers"
	"github.com/cloudfoundry/cf-acceptance-tests/helpers/skip_messages"

	. "github.com/cloudfoundry/cf-acceptance-tests/cats_suite_helpers"
	"github.com/cloudfoundry/cf-acceptance-tests/helpers/assets"
	"github.com/cloudfoundry/cf-acceptance-tests/helpers/random_name"
	. "github.com/cloudfoundry/cf-acceptance-tests/helpers/v3_helpers"
	"github.com/cloudfoundry/cf-test-helpers/v2/cf"
	"github.com/cloudfoundry/cf-test-helpers/v2/helpers"
	"github.com/mholt/archiver/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gbytes"
	. "github.com/onsi/gomega/gexec"
)

var (
	staticFileZip = "assets/staticfile.zip"
)

var _ = V3Describe("deployment", func() {

	var (
		appName              string
		appGuid              string
		dropletGuid          string
		spaceGuid            string
		spaceName            string
		token                string
		instances            int
		webProcess           Process
		stopCheckingAppAlive chan<- bool
		appCheckerIsDone     <-chan bool
	)

	BeforeEach(func() {
		if !Config.GetIncludeDeployments() {
			Skip(skip_messages.SkipDeploymentsMessage)
		}

		appName = random_name.CATSRandomName("APP")
		spaceName = TestSetup.RegularUserContext().Space
		spaceGuid = GetSpaceGuidFromName(spaceName)
		appGuid = CreateApp(appName, spaceGuid, `{"foo":"bar"}`)
		token = GetAuthToken()
		dropletGuid = uploadDroplet(appGuid, assets.NewAssets().DoraZip, Config.GetRubyBuildpackName(), token)
		AssignDropletToApp(appGuid, dropletGuid)

		processes := GetProcesses(appGuid, appName)
		webProcess = GetProcessByType(processes, "web")

		CreateAndMapRoute(appGuid, Config.GetAppsDomain(), appName)
		instances = 2
		ScaleApp(appGuid, instances)

		StartApp(appGuid)
		Expect(string(cf.Cf("apps").Wait().Out.Contents())).To(MatchRegexp(fmt.Sprintf("(v3-)?(%s)*(-web)?(\\s)+(started)", webProcess.Name)))

		By("waiting until all instances are running")
		Eventually(func() int {
			guid := GetProcessGuidForType(appGuid, "web")
			Expect(guid).ToNot(BeEmpty())
			return GetRunningInstancesStats(guid)
		}, Config.CfPushTimeoutDuration()).Should(Equal(instances))

		By("Creating a second droplet for the app")
		makeStaticFileZip()
		dropletGuid = uploadDroplet(appGuid, staticFileZip, Config.GetStaticFileBuildpackName(), token)
	})

	AfterEach(func() {
		app_helpers.AppReport(appName)
		DeleteApp(appGuid)
		os.Remove("assets/staticfile.zip")
	})

	Describe("Deployment", func() {
		BeforeEach(func() {
			By("Assigning a second droplet for the app")
			AssignDropletToApp(appGuid, dropletGuid)
			stopCheckingAppAlive, appCheckerIsDone = checkAppRemainsAlive(appName)
		})

		AfterEach(func() {
			stopCheckingAppAlive <- true
			<-appCheckerIsDone
		})

		It("deploys an app with no downtime", func() {
			Eventually(func() string {
				return helpers.CurlAppRoot(Config, appName)
			}).Should(ContainSubstring("Hi, I'm Dora"))

			_, originalWorkerStartEvent := GetLastAppUseEventForProcess("worker", "STARTED", "")
			originalProcessGuid := GetProcessGuidsForType(appGuid, "web")[0]

			deploymentGuid := CreateDeployment(appGuid)
			Expect(deploymentGuid).ToNot(BeEmpty())

			Eventually(func() int { return len(GetProcessGuidsForType(appGuid, "web")) }, Config.CfPushTimeoutDuration()).
				Should(BeNumerically(">", 1))

			intermediateProcessGuid := GetProcessGuidsForType(appGuid, "web")[1]

			secondDeploymentGuid := CreateDeployment(appGuid)
			Expect(secondDeploymentGuid).ToNot(BeEmpty())

			Eventually(func() int { return GetRunningInstancesStats(originalProcessGuid) }, Config.CfPushTimeoutDuration()).
				Should(BeNumerically("<", instances))

			Eventually(func() []string {
				return GetProcessGuidsForType(appGuid, "web")
			}). // there should eventually be a different final process guid from the first 2
				Should(ContainElement(Not(SatisfyAny(
					Equal(originalProcessGuid),
					Equal(intermediateProcessGuid)))))

			guids := GetProcessGuidsForType(appGuid, "web")
			finalProcessGuid := guids[len(guids)-1]

			Eventually(func() []string {
				return GetProcessGuidsForType(appGuid, "web")
			}).Should(ConsistOf(finalProcessGuid))

			Eventually(func() int {
				return GetRunningInstancesStats(finalProcessGuid)
			}).Should(Equal(instances))

			counter := 0
			Eventually(func() int {
				if strings.Contains(helpers.CurlAppRoot(Config, appName), "Hello from a staticfile") {
					counter++
				} else {
					counter = 0
				}
				return counter
			}).Should(Equal(10))

			Eventually(func() bool {
				restartEventExists, _ := GetLastAppUseEventForProcess("worker", "STARTED", originalWorkerStartEvent.Guid)
				return restartEventExists
			}).Should(BeTrue(), "Did not find a start event indicating the 'worker' process restarted")
		})
	})

	Describe("cancelling deployments", func() {
		It("rolls back to the previous droplet", func() {
			By("creating a deployment with the second droplet")
			originalProcessGuid := GetProcessGuidsForType(appGuid, "web")[0]

			deploymentGuid := CreateDeploymentForDroplet(appGuid, dropletGuid)
			Expect(deploymentGuid).ToNot(BeEmpty())

			By("waiting until there is a second web process  with instances before canceling")
			Eventually(func() int { return len(GetProcessGuidsForType(appGuid, "web")) }, Config.CfPushTimeoutDuration()).
				Should(BeNumerically(">", 1))

			intermediateProcessGuid := GetProcessGuidsForType(appGuid, "web")[1]

			Eventually(func() int { return GetRunningInstancesStats(intermediateProcessGuid) }).
				Should(BeNumerically(">", 0))

			Eventually(func() int { return GetRunningInstancesStats(originalProcessGuid) }).
				Should(BeNumerically("<", instances))

			By("canceling the deployment")
			CancelDeployment(deploymentGuid)

			By("waiting until there is no second web process")
			Eventually(func() int { return len(GetProcessGuidsForType(appGuid, "web")) }).
				Should(Equal(1))

			Eventually(func() int {
				return GetRunningInstancesStats(originalProcessGuid)
			}).Should(Equal(instances))

			counter := 0
			Eventually(func() int {
				if strings.Contains(helpers.CurlAppRoot(Config, appName), "Hi, I'm Dora") {
					counter++
				} else {
					counter = 0
				}
				return counter
			}).Should(Equal(10))
		})
	})

	Describe("Canary deployments", func() {
		It("deploys an app, transitions to pause, is continued and then deploys successfully", func() {
			By("Pushing a canary deployment")

			Eventually(func() string {
				return helpers.CurlAppRoot(Config, appName)
			}).Should(ContainSubstring("Hi, I'm Dora"))

			Expect(cf.Cf("push", appName, "--strategy", "canary", "-b", Config.GetStaticFileBuildpackName(), "-p", staticFileZip).Wait(Config.CfPushTimeoutDuration())).To(Exit(0))

			Eventually(func(g Gomega) {
				session := cf.Cf("app", appName).Wait()
				g.Expect(session).Should(Say("Active deployment with status PAUSED"))
				g.Expect(session).Should(Say("strategy:        canary"))
				g.Expect(session).Should(Exit(0))
			  }).Should(Succeed())

			By("Checking that both the canary and original apps exist simultaneously")
			Eventually(func() string {
				return helpers.CurlAppRoot(Config, appName)
			}).Should(ContainSubstring("Hello from a staticfile"))

			Eventually(func() string {
				return helpers.CurlAppRoot(Config, appName)
			}).Should(ContainSubstring("Hi, I'm Dora"))

			By("Continuing the deployment")
			Expect(cf.Cf("continue-deployment", appName).Wait(Config.CfPushTimeoutDuration())).To(Exit(0))

			By("Verifying the continue succeeded and we rolled out the new process")
			counter := 0
			Eventually(func() int {
				if strings.Contains(helpers.CurlAppRoot(Config, appName), "Hello from a staticfile") {
					counter++
				} else {
					counter = 0
				}
				return counter
			}).Should(Equal(10))

			session := cf.Cf("app", appName).Wait()
			Expect(session).ShouldNot(Say("Active deployment"))
		})

		It("deploys an app, transitions to pause and can be cancelled", func() {
			By("Pushing a canary deployment")

			Eventually(func() string {
				return helpers.CurlAppRoot(Config, appName)
			}).Should(ContainSubstring("Hi, I'm Dora"))

			Expect(cf.Cf("push", appName, "--strategy", "canary", "-b", Config.GetStaticFileBuildpackName(), "-p", staticFileZip).Wait(Config.CfPushTimeoutDuration())).To(Exit(0))

			Eventually(func(g Gomega) {
				session := cf.Cf("app", appName).Wait()
				g.Expect(session).Should(Say("Active deployment with status PAUSED"))
				g.Expect(session).Should(Say("strategy:        canary"))
				g.Expect(session).Should(Exit(0))
			  }).Should(Succeed())

			By("Checking that both the canary and original apps exist simultaneously")
			Eventually(func() string {
				return helpers.CurlAppRoot(Config, appName)
			}).Should(ContainSubstring("Hello from a staticfile"))

			Eventually(func() string {
				return helpers.CurlAppRoot(Config, appName)
			}).Should(ContainSubstring("Hi, I'm Dora"))

			By("Cancelling the deployment")
			Expect(cf.Cf("cancel-deployment", appName).Wait(Config.CfPushTimeoutDuration())).To(Exit(0))

			By("Verifying the cancel succeeded and we rolled back to old process")
			counter := 0
			Eventually(func() int {
				if strings.Contains(helpers.CurlAppRoot(Config, appName), "Hi, I'm Dora") {
					counter++
				} else {
					counter = 0
				}
				return counter
			}).Should(Equal(10))

			session := cf.Cf("app", appName).Wait()
			Expect(session).ShouldNot(Say("Active deployment"))
		})
	})

	FDescribe("max-in-flight deployments", func() {
		It("deploys an app with max_in_flight with a rolling deployment", func() {
			By("Pushing a new rolling deployment with max in flight of 4")
			Eventually(func() string {
				return helpers.CurlAppRoot(Config, appName)
			}).Should(ContainSubstring("Hi, I'm Dora"))

			Expect(cf.Cf("push", appName, "--strategy", "rolling", "--max-in-flight", "4", "-b", Config.GetStaticFileBuildpackName(), "-p", staticFileZip).Wait(Config.CfPushTimeoutDuration())).To(Exit(0))

			Eventually(func() int { return len(GetProcessGuidsForType(appGuid, "web")) }, Config.CfPushTimeoutDuration()).Should(BeNumerically(">", 1))

			processGuids := GetProcessGuidsForType(appGuid, "web")
			newDeploymentGuid := processGuids[len(processGuids)-1]

			By("Ensuring that the new process starts at 4")
			Consistently(func() int {
				return GetProcessByGuid(newDeploymentGuid).Instances
			}).Should(Equal(4))

			Eventually(func() int {
				return GetRunningInstancesStats(newDeploymentGuid)
			}).Should(Equal(instances))
		})

		// It("deploys an app with max_in_flight after a canary deployment has been continued", func() {
		// 	By("Pushing a canary deployment")
		// 	Eventually(func() string {
		// 		return helpers.CurlAppRoot(Config, appName)
		// 	}).Should(ContainSubstring("Hi, I'm Dora"))

		// 	deploymentGuid := CreateDeployment(appGuid, "canary", 4)
		// 	Expect(deploymentGuid).ToNot(BeEmpty())

		// 	Eventually(func() int { return len(GetProcessGuidsForType(appGuid, "web")) }, Config.CfPushTimeoutDuration()).
		// 		Should(BeNumerically(">", 1))

		// 	By("Waiting for the a canary deployment to be paused")
		// 	WaitUntilDeploymentReachesStatus(deploymentGuid, "ACTIVE", "PAUSED")

		// 	processGuids := GetProcessGuidsForType(appGuid, "web")
		// 	newDeploymentGuid := processGuids[len(processGuids)-1]

		// 	By("Continuing the deployment")
		// 	ContinueDeployment(deploymentGuid)
		// 	Eventually(func() int {
		// 		return GetProcessByGuid(newDeploymentGuid).Instances
		// 	}).ShouldNot(Equal(1))

		// 	By("Ensuring that the new process continues at max-in-flight 4")
		// 	Consistently(func() int {
		// 		return GetProcessByGuid(newDeploymentGuid).Instances
		// 	}).Should(Equal(4))

		// 	Eventually(func() int {
		// 		return GetRunningInstancesStats(newDeploymentGuid)
		// 	}).Should(Equal(instances))
		// })
	})
})

func checkAppRemainsAlive(appName string) (chan<- bool, <-chan bool) {
	doneChannel := make(chan bool, 1)
	appCheckerIsDone := make(chan bool, 1)
	ticker := time.NewTicker(1 * time.Second)
	tickerChannel := ticker.C

	go func() {
		defer GinkgoRecover()
		for {
			select {
			case <-doneChannel:
				ticker.Stop()
				appCheckerIsDone <- true
				return
			case <-tickerChannel:
				Expect(helpers.CurlAppRoot(Config, appName)).ToNot(ContainSubstring("404"))
			}
		}
	}()

	return doneChannel, appCheckerIsDone
}

func makeStaticFileZip() {
	staticFiles, err := os.ReadDir(assets.NewAssets().Staticfile)
	Expect(err).NotTo(HaveOccurred())

	var staticFileNames []string
	for _, staticFile := range staticFiles {
		staticFileNames = append(staticFileNames, assets.NewAssets().Staticfile+"/"+staticFile.Name())
	}

	zip := archiver.NewZip()
	err = zip.Archive(staticFileNames, staticFileZip)
	Expect(err).NotTo(HaveOccurred())
}

func uploadDroplet(appGuid, zipFile, buildpackName, token string) string {
	packageGuid := CreatePackage(appGuid)
	url := fmt.Sprintf("%s%s/v3/packages/%s/upload", Config.Protocol(), Config.GetApiEndpoint(), packageGuid)

	UploadPackage(url, zipFile, token)
	WaitForPackageToBeReady(packageGuid)

	buildGuid := StageBuildpackPackage(packageGuid, buildpackName)
	WaitForBuildToStage(buildGuid)
	return GetDropletFromBuild(buildGuid)
}
