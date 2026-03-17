use rand::prelude::IndexedRandom;

const ADJECTIVES: &[&str] = &[
    "brisk", "calm", "clever", "curious", "gentle", "nimble", "quiet", "steady", "swift", "wrinkly",
];
const ANIMALS: &[&str] = &[
    "badgers", "bears", "foxes", "geckos", "otters", "pandas", "ravens", "tigers", "whales",
    "wolves",
];

pub fn generate_session_id() -> String {
    let mut rng = rand::rng();
    let adjective = ADJECTIVES.choose(&mut rng).copied().unwrap_or("steady");
    let animal = ANIMALS.choose(&mut rng).copied().unwrap_or("otters");
    format!("{adjective}-{animal}")
}

pub fn generate_thread_id() -> String {
    format!("thread-{}", generate_session_id())
}
