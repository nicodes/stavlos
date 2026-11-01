use super::*;
#[derive(Clone)]
pub struct KeyFlow {
    provider: String,
    label: String,
    console: String,
    note: String,
}
impl KeyFlow {
    pub fn zai() -> Self {
        Self{provider:"zai".into(),label:"Z.ai GLM Coding Plan".into(),console:"https://z.ai/manage-apikey/apikey-list".into(),note:"Create a key for your GLM Coding Plan and paste it here. It is stored in the auth file, like any other login.".into()}
    }
    pub fn kimi() -> Self {
        Self{provider:"kimi".into(),label:"Kimi For Coding".into(),console:"https://www.kimi.ai/code".into(),note:"Create a key in the Kimi Code console — a key from the Kimi API platform is pay-as-you-go and will not reach the plan. It is stored in the auth file, like any other login.".into()}
    }
}
impl Flow for KeyFlow {
    fn provider(&self) -> &str {
        &self.provider
    }
    fn label(&self) -> &str {
        &self.label
    }
    fn methods(&self) -> Vec<Method> {
        vec![Method::new(
            METHOD_API_KEY,
            &format!("{} (API key)", self.label),
        )]
    }
    fn start(&self, method: String, _: CancellationToken) -> FlowFuture<'_, Arc<Pending>> {
        Box::pin(async move {
            if !method.is_empty() && method != METHOD_API_KEY {
                return Err(message(format!(
                    "{} has no {method:?} sign-in: it is a key from the console",
                    self.provider
                )));
            }
            let (sender, receiver) = tokio::sync::mpsc::channel(1);
            Ok(Arc::new(Pending {
                provider: self.provider.clone(),
                method: METHOD_API_KEY.into(),
                url: self.console.clone(),
                instructions: self.note.clone(),
                expires_in: Duration::from_secs(30 * 60),
                key: Some(sender),
                key_receiver: tokio::sync::Mutex::new(Some(receiver)),
                ..Default::default()
            }))
        })
    }
    fn wait(&self, pending: Arc<Pending>, cancel: CancellationToken) -> FlowFuture<'_, Tokens> {
        Box::pin(async move {
            if pending.key.is_none() {
                return Err(message(format!("no {} login in progress", self.provider)));
            }
            let result = async {
                let mut receiver = pending.key_receiver.lock().await;
                receiver.as_mut().unwrap().recv().await
            };
            tokio::select! {biased;_=cancel.cancelled()=>Err(Error::Cancelled),_=tokio::time::sleep(pending.expires_in)=>Err(Error::Expired),key=result=>key.map(|access|Tokens{access,..Default::default()}).ok_or_else(||message("login abandoned"))}
        })
    }
    fn refresh(&self, _: String, _: CancellationToken) -> FlowFuture<'_, Tokens> {
        Box::pin(async {
            Err(message(
                "an API key does not expire and cannot be refreshed; replace it in the console and sign in again",
            ))
        })
    }
}
