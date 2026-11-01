#[tokio::main]
async fn main() {
    if let Err(error) = stavlos_api::entry::run().await {
        eprintln!("stavlos API: {}", error.message);
        std::process::exit(1);
    }
}
